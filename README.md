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

Or build from source via the [`Makefile`](Makefile) (recommended — it
wraps every common workflow):

```bash
make build         # ./bin/bundle-docs + ./bin/docsearch (fts5+semantic)
make test          # go tests across all 3 tag combos + python tests
make refresh       # rebuild DB + images + redeploy compose stack
make help          # full list
```

The project has two independent Go build tags:

| Build command | What you get | Binary size (docsearch) |
|---|---|---|
| `go build -tags "fts5" ./...` | bundle-docs + docsearch with FTS5 search only | ~7 MB |
| `go build -tags "fts5 semantic" ./...` | …plus `docsearch semantic-index`, `docsearch serve`, vector/hybrid search | ~15 MB |

The `fts5` tag enables SQLite's FTS5 module in `mattn/go-sqlite3` and
is required for any useful search (both the legacy `docs_fts` query
and the semantic chunk index use it). The `semantic` tag adds the
chunking, embedding, sqlite-vec, and web-UI machinery on top —
including the `goldmark` dependency for rendering chunk markdown.
Building without `semantic` cleanly omits all that code.

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
# Legacy FTS5 search only:
go build -tags "fts5" -o docsearch ./cmd/docsearch

# Add semantic search (chunked index, vector/hybrid, web UI):
go build -tags "fts5 semantic" -o docsearch ./cmd/docsearch
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

This is an opt-in feature: the build needs `-tags "fts5 semantic"`. The
default `-tags fts5` build omits the chunking, embedding, vector,
and web-UI code paths entirely, so the binary stays small and pulls
no extra Go dependencies (`goldmark`).

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

## Docker compose deployment

The [`deploy/`](deploy/) directory contains a docker compose stack
for serving docsearch on a single host. Target scale is double-digit
concurrent users; no Kubernetes.

### Stack layout

| Service | Image | Role |
|---|---|---|
| `proxy` | `caddy:2-alpine` | Round-robins to `web` replicas with `/api/health`-based active health checks. Exposes the stack on `HOST_PORT` (default `8080`). |
| `web` | `docsearch-web` | Stateless `docsearch serve`. The compiled docsearch binary, the `sqlite-vec` extension, and the populated `dyalog-docs.db` are baked into the image. Scale with `--scale web=N`. |
| `embedder` | `docsearch-embedder` | FastAPI/uvicorn process with `sentence-transformers` and the default `BAAI/bge-small-en-v1.5` model weights preloaded. Single instance serves all `web` replicas via the `embedder:8000` service name. |

The database is read-only at runtime and is baked into the `web`
image, so containers have no volumes and no shared state. Updating
the docs is an image rebuild plus a redeploy.

### Quick start

```bash
make refresh        # build DB inside Docker, build images, recreate containers
make verify         # post-deploy smoke (health, ⎕IO -> Index Origin, /api/version)
# or equivalently:
#   make db        # deploy/dyalog-docs.db, built inside Docker (no host Go/Python needed)
#   make images    # docsearch-web + docsearch-embedder OCI images
#   make up        # docker compose up -d
# browse http://localhost:8080
```

Day-two operations (rollback, broken-search triage, embedder
recovery, scaling) live in
[`deploy/RUNBOOK.md`](deploy/RUNBOOK.md).

Scale: `docker compose up -d --scale web=5`.

Pin the upstream docs revision: `make db DOCS_REF=<sha>`. The
resolved SHA is written into the DB's `meta` table and surfaced by
`docsearch version` (and `GET /api/version`).

Full build/runtime knobs (registry push, multi-arch, env vars, the
embedder-scaling pattern when one isn't enough) are in
[`deploy/README.md`](deploy/README.md).

### Versioning

Every binary built via the Makefile is stamped with the git SHA and
build timestamp:

```sh
$ docsearch version
docsearch  4a1f2c8
built      2026-05-21T11:00:00Z
go         go1.24.0
docs ref   4696183d2f8a9b1c3e7f
docs repo  https://github.com/Dyalog/documentation.git
docs built 2026-05-21T10:30:00Z
```

The same info is available as JSON on `GET /api/version`, which is
useful for the proxy or an external monitor to confirm what's
actually deployed in each `web` replica.

### Hardening state — `docsearch-web`

Current security posture of `docsearch serve` as shipped in the
`docsearch-web` image.

| Concern | State |
|---|---|
| SQL injection | All queries use bound parameters via `database/sql`. The one `fmt.Sprintf`-built SQL string (`VectorSchemaSQL`) interpolates a fixed integer dimension. |
| Chunk-render XSS | Goldmark output is sanitized with [`bluemonday`](https://github.com/microcosm-cc/bluemonday)'s `UGCPolicy` plus explicit allow-listing of `class` attributes on heading/code/table elements. `<script>`, `<iframe>`, `<object>`, `<embed>`, all `on*` event handlers, `javascript:`/`vbscript:`/`data:` URLs (except `data:image/*` for `<img>`), and SVG-embedded scripts are stripped. |
| Panic isolation | `recoverPanics` middleware catches handler panics, returns a clean `{"error":"internal server error"}`, and logs a structured ERROR record with the panic value and stack trace. The original error never reaches the wire. |
| Response headers | Every response carries `Content-Security-Policy`, `X-Content-Type-Options: nosniff`, `X-Frame-Options: DENY`, and `Referrer-Policy: no-referrer`. CSP is `default-src 'none'` on `/api/*` and `default-src 'self'` (with `'unsafe-inline'` for the page's own inline `<style>` and `<script>` blocks) on `/`. |
| Server error responses | 500s return a generic `{"error":"internal server error"}` body. The underlying error is logged server-side. SQL state, filesystem paths, and embedder URLs are not echoed back to clients. |
| Path traversal | `/api/chunk/<id>` extracts the id via `strings.TrimPrefix` + `strconv.ParseInt`; non-numeric, negative, or slash-bearing input is rejected with 400. SQL `?` bind makes filesystem access from URL parameters impossible regardless. |
| Query length | `q` on `/api/search` is capped at 256 characters. |
| `/api/health` | Returns `{"status":"ok","vector_ready":<bool>}` only. Configuration (embedding URL, DB path) is not exposed. |
| SSRF | The embedder URL is operator-configured via `-embedding-url` or `DOCSEARCH_EMBEDDING_URL`; it is never sourced from request data. |
| CSRF | The API is read-only. No state-changing endpoints exist. |
| Cookies / sessions | None used. |
| Container user | Both `web` and `embedder` run as a non-root UID (`10001`). |
| Container filesystem | No volumes mounted. DB, sqlite-vec extension, and model weights are baked into the image and read-only at runtime. |
| Build context | `.dockerignore` excludes `.venv/`, `.git/`, `.claude/`, `.skis/`, and local `bin/` artifacts so secrets and large local state never enter the image build. |
| TLS | Not configured. Caddy runs HTTP-only inside the stack. For public exposure, drop `auto_https off` from the `Caddyfile` and assign a hostname to enable Let's Encrypt. |
| Authentication / authorization | None. The stack assumes a known trusted group of users. Front with `oauth2-proxy`, an SSO sidecar, or your existing reverse proxy for public access. |
| Rate limiting | None. |

### Operational behaviour — `docsearch-web`

| Concern | State |
|---|---|
| Startup sanity | At boot, `SELECT count(*) FROM docs` and `SELECT count(*) FROM chunks` run with a 3 s timeout. Process refuses to start if `docs` is empty (almost always indicates a wrong `-d` path) and logs the counts otherwise. |
| Embedding model consistency | The semantic indexer records the embedding model name into `meta.embedding_model`. At serve startup, if the DB's recorded model differs from the configured `-embedding-model`, the process refuses to start (vector queries against mismatched models silently return garbage). |
| Post-deploy smoke | `make verify` (`deploy/verify.sh`) probes `/api/health`, runs a canonical `⎕IO` search, and reads `/api/version`. Retries the search up to 5× with 3 s gaps to absorb embedder cold-start. Exits non-zero on any deviation. |
| Liveness probe | `GET /api/health` actually queries the database (`SELECT count(*) FROM docs` with a 2 s timeout). Returns `503 status=down` if the DB is unreachable; `200 status=ok` if everything works; `200 status=degraded` when the DB is fine but `vector_ready=false` so the proxy keeps the replica in rotation for FTS-only traffic. |
| Per-request timeouts | `/api/search` runs with a 30 s context deadline; `/api/chunk/<id>` with 5 s; `/api/health` with 2 s. Client `AbortController` disconnects also propagate via `r.Context()` cancellation. |
| Concurrent reads | SQLite connection pool capped at 8 open connections. `sqlite-vec` is loaded automatically on every new connection through a `ConnectHook`-backed driver, so multiple search requests don't serialise on a single pinned connection. |
| Access log | One structured JSON line per request via `log/slog` (method, path, status, duration, bytes, `req_id`, remote). `/api/health` and `/api/version` are sampled out to keep monitoring noise down. |
| Request correlation | Every response carries an `X-Request-ID` header matching the `req_id` field in the access log, so user-reported issues can be traced to specific log lines. |
| Graceful shutdown | `SIGINT` / `SIGTERM` triggers `http.Server.Shutdown` with a 5 s drain window. Logs `received shutdown signal, draining in-flight requests` then exits cleanly. Pairs with `docker compose down`'s 10 s grace period so in-flight requests get a chance to finish. |
| Embedder retries | `HTTPEmbeddingClient` retries up to 3 times with exponential backoff (200 ms → 600 ms → 1.8 s) on transient errors: connection refused/reset, DNS hiccups, 5xx responses, EOF mid-decode. Does *not* retry 4xx, context cancellation, or response-shape errors. |

### Hardening state — `docsearch-embedder`

The embedder is exposed only via `expose: 8000` in `compose.yaml`,
so it is reachable only from other containers on the docker network
(typically just `web`). The list below describes its posture for
in-network callers.

| Concern | State |
|---|---|
| Network exposure | No host port mapping. Reachable from other containers on the docker network only. The default bind in the container is `0.0.0.0:8000`. |
| Request shape | Pydantic v2 models on `/embed` reject malformed input with `422` and structured detail. |
| Input caps | `texts` capped at 64 items per request; each text capped at 8192 characters; `model` name capped at 256 characters. Values configured in `scripts/embedding-server.py` as `MAX_TEXTS_PER_REQUEST`, `MAX_TEXT_BYTES`, `MAX_MODEL_NAME_BYTES`. |
| Model allowlist | `/embed` rejects any model id not in the configured allowlist with `400`. Default allowlist is the single model the embedder was started with (`--model`); extend with `--allow-model` (repeatable) or the `EMBEDDING_ALLOWED_MODELS` env var (comma-separated). Prevents a compromised caller from coaxing the embedder into downloading arbitrary HF weights. |
| Server error responses | `/embed` 500s return `{"detail":"internal server error"}`. The underlying exception is logged via `logger.exception` server-side; filesystem paths, library tracebacks, and model state are not echoed back. |
| Inference concurrency | One worker thread per process. Concurrent requests queue at the executor rather than racing on the same PyTorch model. |
| `/healthz` | Always `200 {"status":"ok"}`; no per-request work, safe for liveness probes. |
| `/readyz` | `503` until the preloaded model is in memory, then `200 {"status":"ready","models":[...]}`. Exposes the *loaded* model ids, not the allowlist. |
| Container user | Runs as a non-root UID (`10001`). |
| Container filesystem | No volumes; model weights live in the image's `$HF_HOME` (`/opt/huggingface`), owned by the embedder user and read-only at runtime. |
| SQL | None. |
| Shell execution | None. |
| Filesystem access from requests | None. The `model` field flows only into `SentenceTransformer(name)`, which is gated by the allowlist before any filesystem or network lookup. |
| Cookies / sessions / CSRF | No cookies, no sessions, no state-changing endpoints. |
| TLS | Not configured. Internal docker traffic only. |
| Authentication / authorization | None. The trust boundary is the docker network. A compromised `web` container can reach `/embed`, but the allowlist + input caps bound the damage. |
| Rate limiting | None. |

## Continuous integration

Two workflows under [`.github/workflows/`](.github/workflows/):

- [`test.yml`](.github/workflows/test.yml) — runs `make vet` and
  `make test-go` (all three Go build configurations) plus the
  Python embedder tests on every push and PR. ~3 minutes.
- [`refresh-docs.yml`](.github/workflows/refresh-docs.yml) — weekly
  cron (Mondays 06:00 UTC) and manual `workflow_dispatch`. Rebuilds
  the DB and pushes dated images (`docsearch-web:YYYY-MM-DD`,
  `docsearch-embedder:YYYY-MM-DD`) plus `:latest` to GHCR. After it
  runs, deploy hosts just need `docker compose pull && up -d` to
  ship the refresh.

Dependabot is configured in [`dependabot.yml`](.github/dependabot.yml)
for Go modules, pip, Docker base images, and the workflow actions
themselves. Minor + patch updates land as one grouped PR per
ecosystem per week.

## Releases

Pre-built databases are available on the [Releases page](https://github.com/xpqz/bundle-docs/releases).

To create a new release:

```bash
git tag v1.0.0
git push origin v1.0.0
```

This triggers a GitHub Action that builds the tool, generates the database, and publishes it as a release artifact. You can also trigger a snapshot release manually from the [Actions tab](https://github.com/xpqz/bundle-docs/actions).
