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

Or build from source:

```bash
go build .
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
    path TEXT PRIMARY KEY,    -- Navigation breadcrumb (e.g. "Language Reference / Primitive Functions / Iota")
    file TEXT NOT NULL,       -- Relative file path in repo
    content TEXT NOT NULL     -- Markdown content (front-matter stripped, HTML converted)
);

CREATE TABLE help_urls (
    symbol TEXT PRIMARY KEY,  -- APL symbol (e.g. "⍳", ":If")
    path TEXT NOT NULL        -- References docs.path
);
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
