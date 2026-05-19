//go:build semantic

package semanticindex

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

const DefaultEmbeddingModel = "BAAI/bge-small-en-v1.5"

type Embedder interface {
	Embed(context.Context, []string) (EmbeddingBatch, error)
}

type EmbeddingBatch struct {
	Model      string
	Dimensions int
	Embeddings [][]float32
}

type HTTPEmbeddingClient struct {
	URL        string
	Model      string
	HTTPClient *http.Client
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
	model := c.Model
	if model == "" {
		model = DefaultEmbeddingModel
	}
	if c.URL == "" {
		return EmbeddingBatch{}, fmt.Errorf("embedding URL is required")
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
		client = &http.Client{Timeout: 60 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return EmbeddingBatch{}, fmt.Errorf("call embedding service: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return EmbeddingBatch{}, fmt.Errorf("embedding service returned %s: %s", resp.Status, string(detail))
	}

	var decoded embeddingResponse
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
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
