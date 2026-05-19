//go:build fts5

package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"log"

	"github.com/xpqz/bundle-docs/internal/semanticindex"
	"github.com/xpqz/bundle-docs/internal/semanticsearch"
	"github.com/xpqz/bundle-docs/internal/semanticstore"
)

func maybeRunSemanticIndex(args []string) bool {
	if len(args) <= 1 || args[1] != "semantic-index" {
		return false
	}
	runSemanticIndex(args[2:])
	return true
}

func runSemanticIndex(args []string) {
	fs := flag.NewFlagSet("semantic-index", flag.ExitOnError)
	dbPath := fs.String("d", defaultDBPath(), "database path")
	embeddingURL := fs.String("embedding-url", defaultEmbeddingURLValue(), "local embedding HTTP endpoint (env: "+envEmbeddingURL+")")
	embeddingModel := fs.String("embedding-model", semanticindex.DefaultEmbeddingModel, "embedding model name")
	vectorExtension := fs.String("vector-extension", defaultVectorExtension(), "sqlite-vec loadable extension path (env: "+envVectorExtension+")")
	batchSize := fs.Int("batch-size", 32, "texts per embedding request")
	vectorDims := fs.Int("vector-dims", 384, "embedding dimensions")
	maxTokens := fs.Int("chunk-max-tokens", semanticindex.DefaultChunkOptions().MaxTokens, "maximum approximate tokens per chunk")
	if err := fs.Parse(args); err != nil {
		log.Fatal(err)
	}
	if *embeddingURL == "" {
		log.Fatal("semantic-index requires -embedding-url")
	}
	if *vectorExtension == "" {
		log.Fatal("semantic-index requires -vector-extension pointing to sqlite-vec")
	}

	db, err := sql.Open("sqlite3", *dbPath)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	if err := semanticstore.LoadVectorExtension(db, *vectorExtension); err != nil {
		log.Fatal(err)
	}

	stats, err := semanticindex.IndexDatabase(context.Background(), db, semanticindex.HTTPEmbeddingClient{
		URL:   *embeddingURL,
		Model: *embeddingModel,
	}, semanticindex.IndexOptions{
		Chunk:      semanticindex.ChunkOptions{MaxTokens: *maxTokens},
		BatchSize:  *batchSize,
		VectorDims: *vectorDims,
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("semantic index: documents=%d chunks=%d embeddings=%d\n", stats.Documents, stats.Chunks, stats.Embeddings)
}

func runSemanticSearch(db *sql.DB, query, mode, embeddingURL, embeddingModel, vectorExtension string, vectorDims int, fallbackVector bool, limit int) {
	var embedder semanticindex.Embedder
	if mode == string(semanticsearch.ModeVector) || mode == string(semanticsearch.ModeHybrid) {
		if embeddingURL == "" {
			log.Fatalf("semantic %s mode requires -embedding-url", mode)
		}
		if !fallbackVector {
			if vectorExtension == "" {
				log.Fatalf("semantic %s mode requires -vector-extension pointing to sqlite-vec", mode)
			}
			if err := semanticstore.LoadVectorExtension(db, vectorExtension); err != nil {
				log.Fatal(err)
			}
		}
		embedder = semanticindex.HTTPEmbeddingClient{
			URL:   embeddingURL,
			Model: embeddingModel,
		}
	}

	results, err := semanticsearch.SearchChunks(context.Background(), db, embedder, semanticsearch.SearchOptions{
		Query:               query,
		Mode:                semanticsearch.Mode(mode),
		Limit:               limit,
		VectorDims:          vectorDims,
		UseFallbackVectorDB: fallbackVector,
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Print(semanticsearch.FormatResults(results))
}
