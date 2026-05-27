//go:build semantic

package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSemanticIndexCommandIsDocumentedInHelp(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "docsearch")
	build := exec.Command("go", "build", "-tags", "fts5 semantic", "-o", exe, ".")
	build.Env = append(os.Environ(), "CGO_ENABLED=1")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build docsearch: %v\n%s", err, out)
	}

	cmd := exec.Command(exe)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("docsearch without args succeeded, want help exit")
	}
	help := string(out)
	for _, want := range []string{"semantic-index", "-embedding-url", "-embedding-model", "BAAI/bge-small-en-v1.5"} {
		if !strings.Contains(help, want) {
			t.Fatalf("help output missing %q:\n%s", want, help)
		}
	}
}

func TestSemanticIndexCommandRequiresVectorExtension(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "docsearch")
	build := exec.Command("go", "build", "-tags", "fts5 semantic", "-o", exe, ".")
	build.Env = append(os.Environ(), "CGO_ENABLED=1")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build docsearch: %v\n%s", err, out)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer server.Close()

	cmd := exec.Command(exe, "semantic-index", "-d", filepath.Join(t.TempDir(), "docs.db"), "-embedding-url", server.URL)
	cmd.Env = append(os.Environ(), "DOCSEARCH_VECTOR_EXTENSION=", "HOME="+t.TempDir())
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("semantic-index without vector extension succeeded:\n%s", out)
	}
	if !strings.Contains(string(out), "-vector-extension") {
		t.Fatalf("semantic-index error = %q, want vector extension guidance", out)
	}
}

func TestSemanticIndexCommandUsesEnvVarForVectorExtension(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "docsearch")
	build := exec.Command("go", "build", "-tags", "fts5 semantic", "-o", exe, ".")
	build.Env = append(os.Environ(), "CGO_ENABLED=1")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build docsearch: %v\n%s", err, out)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer server.Close()

	envPath := filepath.Join(t.TempDir(), "vec0-from-env.dylib")
	cmd := exec.Command(exe, "semantic-index", "-d", filepath.Join(t.TempDir(), "docs.db"), "-embedding-url", server.URL)
	cmd.Env = append(os.Environ(), "DOCSEARCH_VECTOR_EXTENSION="+envPath)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("semantic-index with bogus env vector extension succeeded:\n%s", out)
	}
	if !strings.Contains(string(out), envPath) {
		t.Fatalf("semantic-index error = %q, want env path %q referenced", out, envPath)
	}
}
