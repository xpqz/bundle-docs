#!/usr/bin/env sh
#
# Inside-container half of the dockerized DB build. Runs the
# bundle-docs -> embedder -> semantic-index pipeline in a single
# layer so the final image (or buildx --output) carries the
# populated dyalog-docs.db.
#
# Inputs (env):
#   DB_OUT          path to write the produced DB (default /db/dyalog-docs.db)
#   DOCS_REF        upstream git ref to pin; empty = tip of main
#   EMBEDDER_PORT   port for the in-build embedder (default 18888)

set -eu

DB_OUT="${DB_OUT:-/db/dyalog-docs.db}"
DOCS_REF="${DOCS_REF:-}"
EMBEDDER_PORT="${EMBEDDER_PORT:-18888}"
EMBEDDER_URL="http://127.0.0.1:${EMBEDDER_PORT}/embed"

mkdir -p "$(dirname "$DB_OUT")"

echo "[build-db] bundle-docs ${DOCS_REF:+ref=$DOCS_REF}"
if [ -n "$DOCS_REF" ]; then
  bundle-docs -o "$DB_OUT" -ref "$DOCS_REF"
else
  bundle-docs -o "$DB_OUT"
fi

echo "[build-db] starting embedder on :${EMBEDDER_PORT}"
python /opt/embedder/embedding-server.py \
    --host 127.0.0.1 \
    --port "$EMBEDDER_PORT" \
    >/tmp/embedder.log 2>&1 &
EMBEDDER_PID=$!
trap 'kill $EMBEDDER_PID 2>/dev/null || true' EXIT

deadline=$((`date +%s` + 180))
until curl -fsS "http://127.0.0.1:${EMBEDDER_PORT}/readyz" >/dev/null 2>&1; do
  if [ "`date +%s`" -gt "$deadline" ]; then
    echo "[build-db] embedder failed to come up; embedder log:" >&2
    cat /tmp/embedder.log >&2 || true
    exit 1
  fi
  sleep 1
done

echo "[build-db] populating semantic tables"
docsearch semantic-index \
    -d "$DB_OUT" \
    -embedding-url "$EMBEDDER_URL" \
    -vector-extension /usr/local/lib/vec0.so

kill "$EMBEDDER_PID" 2>/dev/null || true
wait "$EMBEDDER_PID" 2>/dev/null || true
trap - EXIT

# Quick sanity probe so the build fails loudly on an empty DB.
COUNTS=$(sqlite3 "$DB_OUT" "SELECT (SELECT COUNT(*) FROM docs)||' docs, '||(SELECT COUNT(*) FROM chunks)||' chunks, '||(SELECT COUNT(*) FROM chunk_vec)||' embeddings'")
echo "[build-db] OK: $COUNTS"
