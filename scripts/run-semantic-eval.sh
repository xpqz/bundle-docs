#!/usr/bin/env sh
set -eu

if [ "$#" -lt 3 ]; then
  echo "usage: $0 DB_PATH EMBEDDING_URL VECTOR_EXTENSION [MODE]" >&2
  exit 2
fi

DB_PATH=$1
EMBEDDING_URL=$2
VECTOR_EXTENSION=$3
MODE=${4:-hybrid}

run_query() {
  query=$1
  printf '\n## %s\n' "$query"
  if [ "$MODE" = "fts" ]; then
    docsearch -d "$DB_PATH" -s "$query" -semantic-mode "$MODE" -l 5
  else
    docsearch -d "$DB_PATH" -s "$query" -semantic-mode "$MODE" -embedding-url "$EMBEDDING_URL" -vector-extension "$VECTOR_EXTENSION" -l 5
  fi
}

run_query '⎕FIX'
run_query '⎕IO'
run_query ':If'
run_query 'namespace reference evaluation'
run_query 'how do I define a namespace'
run_query 'format numbers as text'
run_query 'find where an array equals a value'
run_query 'difference between each and rank'
run_query 'execute character vector as code'
run_query 'trap errors in a function'
