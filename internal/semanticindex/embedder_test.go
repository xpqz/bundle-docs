//go:build semantic

package semanticindex

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
	"time"
)

func TestHTTPEmbeddingClientSendsBatchModelAndTexts(t *testing.T) {
	var rawRequest []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/embed" {
			t.Fatalf("path = %s, want /embed", r.URL.Path)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read request: %v", err)
		}
		rawRequest = body
		io.WriteString(w, `{"model":"BAAI/bge-small-en-v1.5","dimensions":3,"embeddings":[[1,2,3],[4,5,6]]}`)
	}))
	defer server.Close()

	client := HTTPEmbeddingClient{
		URL:   server.URL + "/embed",
		Model: DefaultEmbeddingModel,
	}

	if DefaultEmbeddingModel != "BAAI/bge-small-en-v1.5" {
		t.Fatalf("DefaultEmbeddingModel = %q, want BAAI/bge-small-en-v1.5", DefaultEmbeddingModel)
	}

	batch, err := client.Embed(context.Background(), []string{"first chunk", "second chunk"})
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	var wire struct {
		Model string   `json:"model"`
		Texts []string `json:"texts"`
	}
	if err := json.Unmarshal(rawRequest, &wire); err != nil {
		t.Fatalf("decode raw request JSON %s: %v", rawRequest, err)
	}
	if wire.Model != "BAAI/bge-small-en-v1.5" || !reflect.DeepEqual(wire.Texts, []string{"first chunk", "second chunk"}) {
		t.Fatalf("request wire = %#v", wire)
	}
	if batch.Model != "BAAI/bge-small-en-v1.5" || batch.Dimensions != 3 {
		t.Fatalf("batch metadata = (%q, %d), want (BAAI/bge-small-en-v1.5, 3)", batch.Model, batch.Dimensions)
	}
	if len(batch.Embeddings) != 2 || batch.Embeddings[1][2] != 6 {
		t.Fatalf("batch embeddings = %#v", batch.Embeddings)
	}
}

func TestHTTPEmbeddingClientReportsServiceErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad request", http.StatusBadRequest)
	}))
	defer server.Close()

	client := HTTPEmbeddingClient{
		URL:         server.URL,
		Model:       DefaultEmbeddingModel,
		MaxAttempts: 1, // disable retry; we're testing surface error mapping
	}

	_, err := client.Embed(context.Background(), []string{"chunk"})
	if err == nil {
		t.Fatal("Embed succeeded, want service error")
	}
}

func TestHTTPEmbeddingClientRetriesOnTransientNetworkErrorAndRecovers(t *testing.T) {
	var hits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&hits, 1)
		if n < 3 {
			// Two flaky responses, then a real one. http.StatusBadGateway
			// is a transient class; the retry loop should grind through
			// these before succeeding.
			http.Error(w, "bad gateway", http.StatusBadGateway)
			return
		}
		writeJSON(t, w, embeddingResponse{
			Model:      DefaultEmbeddingModel,
			Dimensions: 3,
			Embeddings: [][]float32{{1, 2, 3}},
		})
	}))
	defer server.Close()

	client := HTTPEmbeddingClient{
		URL:         server.URL,
		Model:       DefaultEmbeddingModel,
		MaxAttempts: 5,
		RetryDelay:  1 * time.Millisecond,
		RetryFactor: 1.1, // keep the test fast
	}

	batch, err := client.Embed(context.Background(), []string{"only one"})
	if err != nil {
		t.Fatalf("expected success after retries, got %v (after %d hits)", err, hits)
	}
	if atomic.LoadInt32(&hits) != 3 {
		t.Errorf("server hits = %d, want exactly 3", hits)
	}
	if len(batch.Embeddings) != 1 {
		t.Errorf("embeddings = %v, want one", batch.Embeddings)
	}
}

func TestHTTPEmbeddingClientGivesUpAfterMaxAttempts(t *testing.T) {
	var hits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		http.Error(w, "still bad", http.StatusBadGateway)
	}))
	defer server.Close()

	client := HTTPEmbeddingClient{
		URL:         server.URL,
		Model:       DefaultEmbeddingModel,
		MaxAttempts: 3,
		RetryDelay:  1 * time.Millisecond,
		RetryFactor: 1.1,
	}
	_, err := client.Embed(context.Background(), []string{"x"})
	if err == nil {
		t.Fatal("expected error after exhausting retries")
	}
	if atomic.LoadInt32(&hits) != 3 {
		t.Errorf("server hits = %d, want exactly MaxAttempts=3", hits)
	}
}

func TestHTTPEmbeddingClientDoesNotRetryClientErrors(t *testing.T) {
	var hits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		http.Error(w, "model not allowed", http.StatusBadRequest)
	}))
	defer server.Close()

	client := HTTPEmbeddingClient{
		URL:         server.URL,
		Model:       DefaultEmbeddingModel,
		MaxAttempts: 5,
		RetryDelay:  1 * time.Millisecond,
	}
	_, err := client.Embed(context.Background(), []string{"x"})
	if err == nil {
		t.Fatal("expected client error")
	}
	if atomic.LoadInt32(&hits) != 1 {
		t.Errorf("server hits = %d, want exactly 1 (no retry on 4xx)", hits)
	}
}

func TestHTTPEmbeddingClientHonorsContextCancellationDuringBackoff(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "down", http.StatusBadGateway)
	}))
	defer server.Close()

	client := HTTPEmbeddingClient{
		URL:         server.URL,
		Model:       DefaultEmbeddingModel,
		MaxAttempts: 10,
		RetryDelay:  500 * time.Millisecond,
		RetryFactor: 2.0,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := client.Embed(ctx, []string{"x"})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected error from cancelled context")
	}
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		// Acceptable to return the http.Client's wrapped context
		// error; just ensure we didn't wait the full backoff.
		if elapsed > 400*time.Millisecond {
			t.Fatalf("retry loop ignored context cancel: elapsed=%v, err=%v", elapsed, err)
		}
	}
}

func TestHTTPEmbeddingClientValidatesEmbeddingCount(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, embeddingResponse{
			Model:      DefaultEmbeddingModel,
			Dimensions: 3,
			Embeddings: [][]float32{{1, 2, 3}},
		})
	}))
	defer server.Close()

	client := HTTPEmbeddingClient{
		URL:   server.URL,
		Model: DefaultEmbeddingModel,
	}

	_, err := client.Embed(context.Background(), []string{"one", "two"})
	if err == nil {
		t.Fatal("Embed succeeded with mismatched embedding count")
	}
}

func writeJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Fatalf("encode response: %v", err)
	}
}
