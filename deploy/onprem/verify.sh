#!/usr/bin/env bash
#
# Post-deploy smoke test. Probes the running stack through the
# published proxy port and asserts the three things "the deploy
# worked" hinges on:
#
#   - /api/health reports status ok or degraded (anything else means
#     the DB is broken or the process is down)
#   - a verbatim APL query (⎕IO) lands the canonical Index Origin
#     page at top-1 in hybrid mode (catches "container up but DB
#     empty" and "embedder unreachable from web replicas")
#   - /api/version reports a non-empty build_version (catches "image
#     deployed but it's the old binary")
#
# Requires only curl and python3 on the host.
#
# Usage:
#   ./verify.sh                            # against http://localhost:8080
#   HOST=docsearch.example HOST_PORT=80 ./verify.sh

set -euo pipefail

HOST="${HOST:-localhost}"
HOST_PORT="${HOST_PORT:-8080}"
URL="http://${HOST}:${HOST_PORT}"

fail() {
  echo "verify: FAIL: $*" >&2
  exit 1
}

ok() { echo "  ok: $*"; }

echo "verify: probing ${URL}"

# 1. health
health=$(curl --fail --silent --show-error --max-time 5 "${URL}/api/health" || true)
if [[ -z "$health" ]]; then
  fail "/api/health returned no body (proxy down? web container not running?)"
fi
case "$health" in
  *'"status":"ok"'*|*'"status":"degraded"'*)
    ok "/api/health -> ${health}"
    ;;
  *)
    fail "/api/health bad status: ${health}"
    ;;
esac

# 2. canonical search. Retry to tolerate embedder cold-start
# (~10 s on the first ever call against a fresh container).
# ⎕IO is %E2%8E%95IO percent-encoded.
search=""
for attempt in 1 2 3 4 5; do
  if search=$(curl --fail --silent --show-error --max-time 15 \
        "${URL}/api/search?q=%E2%8E%95IO&mode=hybrid&limit=1" 2>/dev/null); then
    break
  fi
  if [[ $attempt -lt 5 ]]; then
    echo "  /api/search attempt ${attempt} failed; retrying in 3 s (embedder cold-start?)"
    sleep 3
  fi
done
[[ -n "$search" ]] || fail "/api/search did not respond after 5 attempts"
top_title=$(echo "$search" | python3 -c \
  'import sys,json; r=json.load(sys.stdin)["results"]; print(r[0]["title"] if r else "")')
if [[ -z "$top_title" ]]; then
  fail "/api/search returned zero results for ⎕IO"
fi
if [[ "$top_title" != *"Index Origin"* ]]; then
  fail "⎕IO top-1 was '${top_title}', expected 'Index Origin ⎕IO'"
fi
ok "⎕IO -> '${top_title}'"

# 3. version
version=$(curl --fail --silent --show-error --max-time 5 "${URL}/api/version") \
  || fail "/api/version unreachable"
build_version=$(echo "$version" | python3 -c \
  'import sys,json; print(json.load(sys.stdin).get("build_version",""))')
if [[ -z "$build_version" || "$build_version" == "unknown" ]]; then
  fail "/api/version reports empty build_version"
fi
ok "/api/version build_version=${build_version}"

echo "verify: OK"
