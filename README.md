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
