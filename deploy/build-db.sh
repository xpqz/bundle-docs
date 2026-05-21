#!/usr/bin/env bash
#
# Produce deploy/dyalog-docs.db with FTS5 + semantic tables populated,
# ready to be baked into the docsearch-web image. Runs on the host
# (uses your local Go toolchain and the project venv); the docker
# build itself does not need to clone the docs repo or run the
# embedder.
#
# Idempotent: if deploy/dyalog-docs.db already exists, re-running
# will overwrite it.

set -euo pipefail

cd "$(dirname "$0")/.."

DB=$(pwd)/deploy/dyalog-docs.db
EMBED_PORT="${EMBED_PORT:-18888}"
EMBED_URL="http://127.0.0.1:${EMBED_PORT}/embed"

if [ ! -d .venv ]; then
  echo "build-db: .venv missing - run \`python -m venv .venv && .venv/bin/pip install -r scripts/requirements-embedding-server.txt\` first" >&2
  exit 1
fi

echo "build-db: building Go binaries"
mkdir -p bin
go build -tags "fts5 semantic" -o bin/bundle-docs .
go build -tags "fts5 semantic" -o bin/docsearch ./cmd/docsearch

# Make sure a sqlite-vec extension is on disk for semantic-index.
EXT=""
for cand in ~/.bundle-docs/vec0.dylib ~/.bundle-docs/vec0.so /usr/local/lib/vec0.so; do
  if [ -f "$cand" ]; then EXT="$cand"; break; fi
done
if [ -z "$EXT" ]; then
  echo "build-db: installing sqlite-vec locally"
  EXT=$(scripts/install-sqlite-vec.sh)
fi
echo "build-db: using sqlite-vec at $EXT"

echo "build-db: producing fresh FTS5 database -> $DB"
./bin/bundle-docs -o "$DB"

echo "build-db: starting embedder on port $EMBED_PORT"
.venv/bin/python scripts/embedding-server.py --port "$EMBED_PORT" \
    >/tmp/build-db-embedder.log 2>&1 &
EMBED_PID=$!
trap 'kill $EMBED_PID 2>/dev/null || true' EXIT

# Wait for /readyz (model load can take ~10s on a cold venv).
deadline=$((SECONDS + 120))
until curl -fsS "http://127.0.0.1:${EMBED_PORT}/readyz" >/dev/null 2>&1; do
  if [ $SECONDS -gt $deadline ]; then
    echo "build-db: embedder failed to come up; see /tmp/build-db-embedder.log" >&2
    exit 1
  fi
  sleep 1
done

echo "build-db: populating semantic tables"
./bin/docsearch semantic-index \
    -d "$DB" \
    -embedding-url "$EMBED_URL" \
    -vector-extension "$EXT"

# Shut the embedder down cleanly.
kill "$EMBED_PID" 2>/dev/null || true
wait "$EMBED_PID" 2>/dev/null || true
trap - EXIT

# Quick sanity probe.
COUNTS=$(sqlite3 "$DB" "SELECT (SELECT COUNT(*) FROM docs)||' docs, '||(SELECT COUNT(*) FROM chunks)||' chunks, '||(SELECT COUNT(*) FROM chunk_vec)||' embeddings'")
echo "build-db: done. $COUNTS"
echo "build-db: image build can now COPY $DB"
