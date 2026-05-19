# bundle-docs

A CLI tool that bundles Dyalog APL documentation into a SQLite database for offline use.

## What it does

1. Clones the [Dyalog documentation repository](https://github.com/Dyalog/documentation)
2. Parses the mkdocs monorepo structure (including nested subsites)
3. Extracts all markdown content with navigation paths
4. Optionally maps APL symbols to their documentation pages
5. Outputs a SQLite database

## Installation

```bash
go install github.com/xpqz/bundle-docs@latest
```

Or build from source (requires FTS5 build tag for full-text search):

```bash
go build -tags "fts5" .
```

## Usage

```bash
# Basic usage - creates dyalog-docs.db
./bundle-docs

# Custom output path
./bundle-docs -o docs.db

# With symbol-to-URL mappings
./bundle-docs -help-urls symbol-urls.json

# Keep the cloned repo for inspection
./bundle-docs -keep

# Use a different documentation repo
./bundle-docs -repo https://github.com/Dyalog/documentation.git
```

## Flags

| Flag | Default | Description |
|------|---------|-------------|
| `-o` | `dyalog-docs.db` | Output database path |
| `-repo` | `git@github.com:Dyalog/documentation.git` | Documentation repo URL |
| `-help-urls` | `symbol-urls.json` | Path to symbol-URLs JSON file |
| `-keep` | `false` | Keep cloned repo (prints path) |

## Database schema

```sql
CREATE TABLE docs (
    path TEXT PRIMARY KEY,    -- Navigation breadcrumb (e.g. "Core Reference / ... / Index Generator")
    file TEXT NOT NULL,       -- Relative file path in repo
    title TEXT NOT NULL,      -- H1 title extracted from the document
    keywords TEXT NOT NULL,   -- Search keywords from hidden divs (e.g. "⍳ iota index")
    content TEXT NOT NULL,    -- Markdown content (front-matter stripped, HTML converted)
    exclude INTEGER NOT NULL  -- 1 for disambiguation pages, 0 otherwise
);

-- FTS5 virtual table for full-text search
CREATE VIRTUAL TABLE docs_fts USING fts5(path, title, keywords, content, content='docs');

CREATE TABLE help_urls (
    symbol TEXT PRIMARY KEY,  -- APL symbol (e.g. "⍳", ":If")
    path TEXT NOT NULL        -- References docs.path
);
```

### Full-text search example

```sql
-- Search for documents mentioning "iota"
SELECT path, title FROM docs_fts WHERE docs_fts MATCH 'iota' LIMIT 10;

-- Search with ranking
SELECT path, title, rank FROM docs_fts WHERE docs_fts MATCH 'primitive function' ORDER BY rank LIMIT 10;

-- Exclude disambiguation pages from results
SELECT d.path, d.title
FROM docs_fts f
JOIN docs d ON f.rowid = d.rowid
WHERE f.docs_fts MATCH 'grade' AND d.exclude = 0;
```

## docsearch

A command-line tool for querying the documentation database.

### Building

```bash
go build -tags "fts5" -o docsearch docsearch.go
```

### Usage

```bash
docsearch -s <search>    # Search for a term
docsearch -r <rowid>     # Fetch document by rowid
```

| Flag | Default | Description |
|------|---------|-------------|
| `-d` | `./dyalog-docs.db` | Database path |
| `-s` | | Search string (use `-` to read from stdin) |
| `-r` | | Fetch document content by rowid |
| `-l` | `10` | Maximum number of results |
| `-semantic-mode` | (off) | `fts`, `vector`, or `hybrid` (see [Semantic search](#semantic-search)) |
| `-embedding-url` | `$DOCSEARCH_EMBEDDING_URL` or `http://localhost:8000/embed` | Local embedding server endpoint |
| `-vector-extension` | `$DOCSEARCH_VECTOR_EXTENSION` or `~/.bundle-docs/vec0.{dylib,so,dll}` | sqlite-vec loadable extension path |

### Search priority

Results are returned in the following order:

1. Exact case-insensitive match on keywords
2. FTS match on title
3. FTS match on content

Duplicates are suppressed; a document appears only once at its highest priority.

### Examples

```bash
# Search for "iota"
./docsearch -s "iota"
86 Index Generator R←⍳Y
2598 Iota ⍳
...

# Search for an APL symbol
./docsearch -s "⍳"
86 Index Generator R←⍳Y
87 Index Of R←X⍳Y

# Read search term from stdin
echo "binomial" | ./docsearch -s -

# Fetch a document by rowid
./docsearch -r 86
# Index Generator R←⍳Y
...
```

## Semantic search

`docsearch` also supports a semantic search mode that combines FTS5 with vector
similarity over local embeddings of the documentation. It is built on
[sqlite-vec](https://github.com/asg017/sqlite-vec) and a small local HTTP
embedding server.

### Prerequisites

1. Install the sqlite-vec loadable extension into `~/.bundle-docs/`:

   ```bash
   scripts/install-sqlite-vec.sh
   # → ~/.bundle-docs/vec0.dylib (macOS) or vec0.so (Linux) or vec0.dll (Windows)
   ```

   `SQLITE_VEC_VERSION` and `INSTALL_DIR` override the defaults.

2. Install and start the local embedding server:

   ```bash
   python -m venv .venv && . .venv/bin/activate
   pip install -r scripts/requirements-embedding-server.txt
   python scripts/embedding-server.py          # http://127.0.0.1:8000/embed
   ```

   The default model is `BAAI/bge-small-en-v1.5` (384-dim, English-only). The
   first call downloads the model into `~/.cache/huggingface`.

### Building the semantic index

After `bundle-docs update` (or any rebuild of `dyalog-docs.db`), populate the
semantic tables:

```bash
docsearch semantic-index
# documents, chunks, chunks_fts, chunk_vec written into the existing DB
```

Override the conventional paths with `-embedding-url`, `-vector-extension`, or
the matching environment variables (see below).

### Querying

```bash
# FTS-only (good for APL glyphs and system names)
docsearch -s '⎕FIX' -semantic-mode fts

# Vector-only (good for natural-language questions)
docsearch -s 'how do I define a namespace' -semantic-mode vector

# Hybrid (default; fuses FTS and vector with query-dependent weighting)
docsearch -s 'namespace reference evaluation' -semantic-mode hybrid
```

`docsearch -r <chunk_id>` returns the chunk text for any semantic result row.

### Environment variables

| Variable | Default | Purpose |
|---|---|---|
| `DOCSEARCH_EMBEDDING_URL` | `http://localhost:8000/embed` | Local embedding server |
| `DOCSEARCH_VECTOR_EXTENSION` | `~/.bundle-docs/vec0.{dylib,so,dll}` (if present) | sqlite-vec loadable extension path |

CLI flags `-embedding-url`, `-vector-extension`, `-embedding-model`, and
`-vector-dims` always override the environment.

### Web interface

`docsearch serve` exposes the same FTS/vector/hybrid search through a small
HTTP API and a single-page HTML UI:

```bash
docsearch serve                         # listens on 127.0.0.1:8080
docsearch serve -addr 0.0.0.0:9090      # bind elsewhere
```

Endpoints:

- `GET /` — embedded search UI
- `GET /api/search?q=<query>&mode={fts,vector,hybrid}&limit=N` (JSON results)
- `GET /api/chunk/<id>` (full chunk markdown)
- `GET /api/health` (extension load status, embedding URL)

The same `-embedding-url` / `-vector-extension` / `DOCSEARCH_*` defaults
apply. The server keeps the sqlite-vec connection pinned, so queries are
serialized; that's fine for single-user local use.

### Evaluation

A representative query set lives in
[`docs/evaluation/semantic-search-queries.md`](docs/evaluation/semantic-search-queries.md).
Run it against any tuning change with:

```bash
scripts/run-semantic-eval.sh \
    ~/.bundle-docs/dyalog-docs.db \
    http://localhost:8000/embed \
    ~/.bundle-docs/vec0.dylib \
    hybrid > semantic-eval.txt
```

The plan and design notes are in [`docs/plans/semantic-search.md`](docs/plans/semantic-search.md).

## symbol-urls.json format

A JSON array mapping APL symbols to documentation URL paths:

```json
[
  {"symbol": "⍳", "url": "language-reference-guide/primitive-functions/index-generator"},
  {"symbol": ":If", "url": "programming-reference-guide/defined-functions-and-operators/traditional-functions-and-operators/control-structures/if"}
]
```

## Requirements

- Go 1.24+
- Git (for cloning the documentation repo)
- CGO enabled (for sqlite3)

## Releases

Pre-built databases are available on the [Releases page](https://github.com/xpqz/bundle-docs/releases).

To create a new release:

```bash
git tag v1.0.0
git push origin v1.0.0
```

This triggers a GitHub Action that builds the tool, generates the database, and publishes it as a release artifact. You can also trigger a snapshot release manually from the [Actions tab](https://github.com/xpqz/bundle-docs/actions).
