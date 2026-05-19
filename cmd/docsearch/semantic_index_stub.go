//go:build !fts5

package main

import (
	"database/sql"
	"log"
)

func maybeRunSemanticIndex(args []string) bool {
	if len(args) <= 1 || args[1] != "semantic-index" {
		return false
	}
	log.Fatal("semantic-index requires building docsearch with -tags fts5")
	return true
}

func runSemanticSearch(db *sql.DB, query, mode, embeddingURL, embeddingModel, vectorExtension string, vectorDims int, fallbackVector bool, limit int) {
	log.Fatal("semantic search requires building docsearch with -tags fts5")
}
