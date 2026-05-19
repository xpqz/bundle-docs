# bundle-docs

Offline search over the Dyalog APL documentation. Bundles the upstream
mkdocs site into a single SQLite database and ships two front-ends:

- **`docsearch`** — CLI for FTS5, semantic (vector), and hybrid search.
- **`docsearch serve`** — local web UI with live-as-you-type search,
  rendered chunk previews, and deep links to the live docs.

## What it does

1. Clones the [Dyalog documentation repository](https://github.com/Dyalog/documentation)
2. Parses the mkdocs monorepo structure (including nested subsites)
3. Extracts all markdown content with navigation paths and H1 titles
4. Optionally maps APL symbols to their documentation pages
5. Outputs a SQLite database with FTS5; `docsearch semantic-index` then
   adds chunked markdown, embeddings, and a `sqlite-vec` index for
   semantic search on top of the same file.

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

### Search priority (legacy `-s` mode)

Without `-semantic-mode`, `docsearch -s` queries the original `docs` /
`docs_fts` tables and returns whole documents in the following order:

1. Exact case-insensitive match on keywords
2. FTS match on title
3. FTS match on content

Duplicates are suppressed; a document appears only once at its highest
priority. For glyph-aware, semantic, or hybrid retrieval — and for the
ranking heuristics described below — use `-semantic-mode` (or the web
UI, which always does).

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

   The server is FastAPI + uvicorn (the underlying PyTorch model isn't
   safe to run concurrently on MPS, but HTTP handling is async so the
   live-search loop doesn't block at the socket). Endpoints:
   `POST /embed` for the inference contract, `GET /healthz` for
   liveness, `GET /readyz` for readiness (200 once the model has
   loaded).

   The default model is `BAAI/bge-small-en-v1.5` (384-dim, English-only). The
   first call downloads the model into `~/.cache/huggingface`.

### Building the semantic index

`bundle-docs update` rebuilds `dyalog-docs.db` from the upstream repo;
it parses the markdown, extracts H1 titles (preferring `<h1>` over
`# ` to match Dyalog's mkdocs convention, with a fence-aware fallback
scanner), and writes the `docs` + `docs_fts` tables. The semantic
tables are not rebuilt automatically — after `update`, run:

```bash
docsearch semantic-index
# documents, chunks, chunks_fts, chunk_vec written into the existing DB
```

Indexing ~3000 documents into ~4800 chunks takes ~2 minutes on Apple
Silicon (MPS). The embedded text for each chunk includes the document
title and section heading along with the body, so primitive pages
with terse bodies (`⎕FIX`, `⎕OR`, `⍳`, ...) match natural-language
queries against the canonical name.

Override the conventional paths with `-embedding-url`,
`-vector-extension`, or the matching environment variables (see below).

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

`docsearch serve` exposes the same FTS / vector / hybrid search through
an HTTP API and a single-page UI:

```bash
docsearch serve                         # listens on 127.0.0.1:8080
docsearch serve -addr 0.0.0.0:9090      # bind elsewhere
```

Open <http://127.0.0.1:8080> in a browser.

UI features:

- **Live-as-you-type.** Results refresh ~250ms after the last keystroke.
  Each request cancels the previous in-flight one via `AbortController`,
  so slow responses can't overwrite faster newer ones.
- **Result titles link to the live docs.** Clicking the title opens
  `https://dyalog.github.io/documentation/20.0/<page>#<anchor>` in a
  new tab — the chunk's anchor takes you to the exact section.
- **Expand button** renders the chunk body inline as HTML via
  [goldmark](https://github.com/yuin/goldmark): code fences, tables,
  lists, inline code, and mkdocs admonitions (`!!! warning "..."`)
  all render properly. Relative `.md` cross-references inside the
  chunk are rewritten to absolute live-docs URLs.
- **One result per document.** Multiple chunks of the same page are
  collapsed to the best-scoring one (see Ranking below for the API
  flag controlling this).
- **Rank badges** show how each result earned its place
  (`fts#9 + vector#4 + canonical + ref + title`).

Endpoints:

| Method | Path | Returns |
|---|---|---|
| GET  | `/` | Embedded HTML UI |
| GET  | `/api/search?q=&mode=&limit=` | JSON results (`mode` is `fts`/`vector`/`hybrid`, default `hybrid`; `limit` 1-50, default 10) |
| GET  | `/api/chunk/<id>` | Chunk markdown + rendered HTML + `source_url` |
| GET  | `/api/health` | `vector_ready`, configured `embedding_url` |

`/api/search` and `/api/chunk/<id>` both include a `source_url`
pointing at the upstream `dyalog.github.io` page (with an anchor
fragment for the section).

The sqlite-vec connection is pinned to one SQL connection per the
extension's threading requirements, so requests serialize at the DB.
HTTP handling is concurrent on the Go runtime — fine for single-user
local development, and the embedding server (FastAPI + uvicorn) does
its own concurrency control on top.

### How ranking works

The badges next to each result show which signals fired:

- **`fts#N`** — FTS5 (bm25) rank N. Exact-looking queries (APL glyphs,
  `:` prefixes, quoted strings) use phrase matching; natural-language
  queries are split into significant tokens combined with OR after
  English stopwords are dropped.
- **`vector#N`** — vector cosine-similarity rank N from the
  sqlite-vec index. The query is embedded with the same model used
  during indexing (default `BAAI/bge-small-en-v1.5`, 384-dim).
- **`canonical`** — small additive bonus when the chunk's heading
  equals (or is contained in) the document title. These chunks are
  the reference body of a page rather than an Examples or Warning
  sub-section.
- **`ref`** — bonus when an exact query lands on a page under
  `Core Reference / Dyalog APL Language /`, so verbatim glyph or
  system-function queries prefer the Language Reference Guide over
  Release Notes coverage of the same symbol.
- **`title`** — large override bonus when an exact query appears
  verbatim in the chunk title or heading (case-insensitive). This
  is what makes `⎕FIX`, `⎕IO`, `⎕OR`, `:If`, `(220⌶)`, `⍳`, etc.
  reliably land their canonical primitive page at top-1.

Hybrid mode fuses FTS and vector ranks with [reciprocal rank
fusion](https://plg.uwaterloo.ca/~gvcormac/cormacksigir09-rrf.pdf),
then applies the bonuses above, then re-sorts. The per-side pool is
expanded to up to 50 candidates before fusion so canonical primitive
pages that sit at FTS rank 8-25 can still surface.

### Evaluation

A representative query set spanning exact APL queries, mixed phrases,
and natural-language questions lives in
[`docs/evaluation/semantic-search-queries.md`](docs/evaluation/semantic-search-queries.md).
The current baseline output is in
[`docs/evaluation/semantic-eval-tuned.txt`](docs/evaluation/semantic-eval-tuned.txt).

Re-run after any tuning change to compare:

```bash
scripts/run-semantic-eval.sh \
    ~/.bundle-docs/dyalog-docs.db \
    http://localhost:8000/embed \
    ~/.bundle-docs/vec0.dylib \
    hybrid > /tmp/semantic-eval.txt

diff docs/evaluation/semantic-eval-tuned.txt /tmp/semantic-eval.txt
```

The plan and design notes are in
[`docs/plans/semantic-search.md`](docs/plans/semantic-search.md).

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
