//go:build !semantic

package main

import (
	"database/sql"
	"log"
)

// These stubs satisfy the main package's references when docsearch is
// built without the "semantic" tag. They produce a clear error if a
// semantic-only subcommand or flag is invoked, while leaving the
// original `docsearch -s <query>` FTS5 flow fully functional.

const semanticDisabledMessage = "semantic features require building docsearch with -tags \"fts5 semantic\""

func maybeRunSemanticIndex(args []string) bool {
	if len(args) <= 1 || args[1] != "semantic-index" {
		return false
	}
	log.Fatal(semanticDisabledMessage)
	return true
}

func maybeRunServe(args []string) bool {
	if len(args) <= 1 || args[1] != "serve" {
		return false
	}
	log.Fatal(semanticDisabledMessage)
	return true
}

func runSemanticSearch(_ *sql.DB, _, _, _, _, _ string, _ int, _ bool, _ int) {
	log.Fatal(semanticDisabledMessage)
}
