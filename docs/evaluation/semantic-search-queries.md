# Semantic Search Evaluation Queries

Plan reference: `docs/plans/semantic-search.md`

Use this set to compare semantic search tuning changes across exact, semantic, and mixed Dyalog documentation queries.

| Query | Type | Expected emphasis |
| --- | --- | --- |
| `⎕FIX` | Exact glyph/system function | FTS |
| `⎕IO` | Exact system variable | FTS |
| `:If` | Exact control structure | FTS |
| `namespace reference evaluation` | Mixed technical phrase | Hybrid |
| `how do I define a namespace` | Natural language | Vector |
| `format numbers as text` | Natural language | Vector |
| `find where an array equals a value` | Natural language | Vector |
| `difference between each and rank` | Mixed technical phrase | Hybrid |
| `execute character vector as code` | Natural language | Vector |
| `trap errors in a function` | Natural language | Vector |

Suggested command shape after building the semantic index:

```sh
docsearch -s '⎕FIX' -semantic-mode fts
docsearch -s 'how do I define a namespace' -semantic-mode vector -embedding-url http://localhost:8000/embed -vector-extension /path/to/sqlite-vec
docsearch -s 'namespace reference evaluation' -semantic-mode hybrid -embedding-url http://localhost:8000/embed -vector-extension /path/to/sqlite-vec
```

Repeatable runner:

```sh
scripts/run-semantic-eval.sh ~/.bundle-docs/dyalog-docs.db http://localhost:8000/embed /path/to/sqlite-vec hybrid > semantic-eval-baseline.txt
```

For each tuning change, rerun the command and compare the top five rows for:

- expected document title or heading in the first result
- stable row ids for exact glyph queries
- useful snippet text
- explanation field showing `fts#`, `vector#`, or both
