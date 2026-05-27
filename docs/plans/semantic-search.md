# Semantic Search Plan

## Goal

Add local semantic search for the Dyalog APL documentation corpus, currently around 3000 medium-sized Markdown files, while keeping SQLite as the storage and query engine.

## Recommendation

Use hybrid retrieval rather than vector search alone:

- Use SQLite FTS5 for exact terms, APL glyphs, system names, control structures, and short technical phrases.
- Use a SQLite vector extension for semantic similarity over embedded documentation chunks.
- Combine the two result sets with score fusion, or use FTS to shortlist and vector search to rerank.

APL documentation contains many exact symbols and names such as `⎕FIX`, `⍳`, `:If`, and `namespace`. These are cases where lexical search is often more reliable than embeddings. Natural-language queries such as "how do I define a namespace" should benefit more from vector search.

## Embedding Model

Start with `BAAI/bge-small-en-v1.5`.

Rationale:

- English-only retrieval model.
- Small enough for modest local hardware.
- Produces 384-dimensional vectors, which keeps SQLite storage and search cheap.
- Better first choice for retrieval quality than `sentence-transformers/all-MiniLM-L6-v2`.

Alternatives:

- `nomic-ai/nomic-embed-text-v1.5` if longer chunks or adjustable vector dimensions become important.
- `sentence-transformers/all-MiniLM-L6-v2` as the smallest and fastest baseline.

## SQLite Vector Extension

Prefer `sqlite-vec` initially.

Rationale:

- Small SQLite extension written in C.
- Practical successor to `sqlite-vss`.
- Works well for local-first applications and modest document collections.

SQLite's `vec1` extension is also worth tracking, especially if it becomes easier to package with the target SQLite runtime.

## Chunking

Do not embed whole files by default. Chunk by Markdown structure.

Suggested approach:

- Split by Markdown headings.
- Keep path, document title, heading, and anchor metadata.
- Target roughly 300-800 tokens per chunk.
- Add small overlap only for unusually long sections.

Expected scale:

- 3000 Markdown files should likely produce around 10k-50k chunks.
- Exact vector search over 384-dimensional vectors should be acceptable at this scale.
- Approximate indexing can be added later if latency requires it.

## Proposed Schema

```sql
CREATE TABLE documents (
  id INTEGER PRIMARY KEY,
  path TEXT NOT NULL UNIQUE,
  title TEXT
);

CREATE TABLE chunks (
  id INTEGER PRIMARY KEY,
  document_id INTEGER NOT NULL REFERENCES documents(id),
  heading TEXT,
  anchor TEXT,
  text TEXT NOT NULL
);

CREATE VIRTUAL TABLE chunks_fts USING fts5(
  title,
  heading,
  text,
  content='',
  tokenize='unicode61'
);

-- Exact syntax depends on the selected vector extension.
-- For sqlite-vec, use a vec0 virtual table with 384-dimensional float vectors.
CREATE VIRTUAL TABLE chunk_vec USING vec0(
  embedding FLOAT[384]
);
```

The FTS table should maintain a stable mapping to `chunks.id`. The vector table should use the same chunk id as its row id where the selected extension supports that pattern.

## Query Strategy

Use query classification to weight retrieval modes:

- Exact-looking queries containing APL glyphs, `⎕` names, `:` control structures, or quoted terms should heavily weight FTS5.
- Natural-language queries should weight vector search more heavily.
- Mixed queries should run both and combine results.

Initial fusion can be simple reciprocal-rank fusion:

```text
score = fts_weight / (rank_offset + fts_rank)
      + vec_weight / (rank_offset + vec_rank)
```

Use a stable `rank_offset`, for example 60, to reduce overfitting to raw scores from either backend.

## Implementation Steps

1. Add dependencies and setup for the chosen embedding runner.
2. Add an indexing command that walks the Markdown corpus, chunks files, and stores documents, chunks, FTS rows, and vectors in SQLite.
3. Add vector-extension loading for search and indexing paths.
4. Add a hybrid search command or extend `docsearch` to query FTS5 plus vector search.
5. Add a small evaluation set of representative Dyalog documentation queries.
6. Tune chunk size, query weighting, and result rendering from the evaluation set.

## Evaluation Queries

Include exact, semantic, and mixed queries:

- `⎕FIX`
- `⎕IO`
- `:If`
- `namespace reference evaluation`
- `how do I define a namespace`
- `format numbers as text`
- `find where an array equals a value`
- `difference between each and rank`
- `execute character vector as code`
- `trap errors in a function`
