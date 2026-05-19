//go:build semantic

package semanticindex

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
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
		http.Error(w, "model unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	client := HTTPEmbeddingClient{
		URL:   server.URL,
		Model: DefaultEmbeddingModel,
	}

	_, err := client.Embed(context.Background(), []string{"chunk"})
	if err == nil {
		t.Fatal("Embed succeeded, want service error")
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
