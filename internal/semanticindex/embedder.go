//go:build semantic

package semanticindex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"syscall"
	"time"
)

const DefaultEmbeddingModel = "BAAI/bge-small-en-v1.5"

// Defaults for HTTPEmbeddingClient. Exposed as named constants so
// tests and the docsearch wiring can override them cleanly.
const (
	DefaultEmbedMaxAttempts = 3
	DefaultEmbedRetryDelay  = 200 * time.Millisecond
	DefaultEmbedRetryFactor = 3.0
	DefaultEmbedTimeout     = 60 * time.Second
)

type Embedder interface {
	Embed(context.Context, []string) (EmbeddingBatch, error)
}

type EmbeddingBatch struct {
	Model      string
	Dimensions int
	Embeddings [][]float32
}

// HTTPEmbeddingClient posts /embed requests to a local embedding
// server. It retries on transient network errors (connection
// refused, DNS failure, EOF before headers, server-side 5xx) up to
// MaxAttempts times with exponential backoff, but stops immediately
// on client errors (4xx), context cancellation, or response-shape
// problems that retrying wouldn't fix.
type HTTPEmbeddingClient struct {
	URL        string
	Model      string
	HTTPClient *http.Client

	// MaxAttempts is the total number of attempts (including the
	// first). Zero falls back to DefaultEmbedMaxAttempts.
	MaxAttempts int
	// RetryDelay is the initial backoff between retries.
	RetryDelay time.Duration
	// RetryFactor multiplies the delay between successive retries.
	RetryFactor float64
}

type embeddingRequest struct {
	Model string   `json:"model"`
	Texts []string `json:"texts"`
}

type embeddingResponse struct {
	Model      string      `json:"model"`
	Dimensions int         `json:"dimensions"`
	Embeddings [][]float32 `json:"embeddings"`
}

func (c HTTPEmbeddingClient) Embed(ctx context.Context, texts []string) (EmbeddingBatch, error) {
	if c.URL == "" {
		return EmbeddingBatch{}, fmt.Errorf("embedding URL is required")
	}

	attempts := c.MaxAttempts
	if attempts <= 0 {
		attempts = DefaultEmbedMaxAttempts
	}
	delay := c.RetryDelay
	if delay <= 0 {
		delay = DefaultEmbedRetryDelay
	}
	factor := c.RetryFactor
	if factor <= 1 {
		factor = DefaultEmbedRetryFactor
	}

	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		batch, err := c.embedOnce(ctx, texts)
		if err == nil {
			return batch, nil
		}
		lastErr = err
		if !isTransientEmbedError(err) || attempt == attempts {
			return EmbeddingBatch{}, err
		}
		// Backoff with context-aware wait so we don't sleep through
		// a client disconnect.
		select {
		case <-ctx.Done():
			return EmbeddingBatch{}, ctx.Err()
		case <-time.After(delay):
		}
		delay = time.Duration(float64(delay) * factor)
	}
	return EmbeddingBatch{}, lastErr
}

// embedOnce performs a single attempt. The error it returns is
// wrapped with a sentinel (transientEmbedError) when the caller may
// safely retry.
func (c HTTPEmbeddingClient) embedOnce(ctx context.Context, texts []string) (EmbeddingBatch, error) {
	model := c.Model
	if model == "" {
		model = DefaultEmbeddingModel
	}

	body, err := json.Marshal(embeddingRequest{Model: model, Texts: texts})
	if err != nil {
		return EmbeddingBatch{}, fmt.Errorf("encode embedding request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, bytes.NewReader(body))
	if err != nil {
		return EmbeddingBatch{}, fmt.Errorf("create embedding request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := c.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: DefaultEmbedTimeout}
	}
	resp, err := client.Do(req)
	if err != nil {
		return EmbeddingBatch{}, markTransient(fmt.Errorf("call embedding service: %w", err))
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		err := fmt.Errorf("embedding service returned %s: %s", resp.Status, string(detail))
		if resp.StatusCode >= 500 && resp.StatusCode != http.StatusNotImplemented {
			return EmbeddingBatch{}, markTransient(err)
		}
		return EmbeddingBatch{}, err
	}

	var decoded embeddingResponse
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		// A torn-down keepalive can manifest as a decode error; if
		// the underlying problem is a network issue we'd like to
		// retry it.
		if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
			return EmbeddingBatch{}, markTransient(fmt.Errorf("decode embedding response: %w", err))
		}
		return EmbeddingBatch{}, fmt.Errorf("decode embedding response: %w", err)
	}
	if len(decoded.Embeddings) != len(texts) {
		return EmbeddingBatch{}, fmt.Errorf("embedding count %d does not match text count %d", len(decoded.Embeddings), len(texts))
	}
	return EmbeddingBatch{
		Model:      decoded.Model,
		Dimensions: decoded.Dimensions,
		Embeddings: decoded.Embeddings,
	}, nil
}

// transientEmbedError is a marker wrapper so the retry loop can
// distinguish "worth retrying" from "give up".
type transientEmbedError struct{ err error }

func (e *transientEmbedError) Error() string { return e.err.Error() }
func (e *transientEmbedError) Unwrap() error { return e.err }

func markTransient(err error) error { return &transientEmbedError{err: err} }

// isTransientEmbedError reports whether the supplied error is one
// the retry loop should treat as worth another attempt. Explicit
// markers from embedOnce win; otherwise we look for the usual
// network-level suspects in case future code paths surface a raw
// net error.
func isTransientEmbedError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var marker *transientEmbedError
	if errors.As(err, &marker) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ECONNRESET) {
		return true
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return true
	}
	return false
}
