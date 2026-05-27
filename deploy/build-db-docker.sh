#!/usr/bin/env bash
#
# Reproducible DB build that needs only Docker on the host. Wraps
# `docker buildx build` against deploy/Dockerfile.dbbuilder and uses
# buildx's local output mode to drop the produced dyalog-docs.db
# back onto the host filesystem.
#
# Usage:
#   ./deploy/build-db-docker.sh
#   DOCS_REF=abc123 ./deploy/build-db-docker.sh    # pin upstream
#   PLATFORMS=linux/arm64,linux/amd64 ./deploy/build-db-docker.sh

set -euo pipefail

cd "$(dirname "$0")/.."

DOCS_REF="${DOCS_REF:-}"
PLATFORMS="${PLATFORMS:-linux/arm64}"
BUILD_VERSION="${BUILD_VERSION:-$(git rev-parse --short HEAD 2>/dev/null || echo dev)}"
BUILD_TIME="${BUILD_TIME:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}"

echo "build-db-docker: platform=$PLATFORMS docs_ref=${DOCS_REF:-tip-of-main}"

docker buildx build \
    --platform "$PLATFORMS" \
    --target db \
    --output "type=local,dest=deploy" \
    -f deploy/Dockerfile.dbbuilder \
    --build-arg "DOCS_REF=$DOCS_REF" \
    --build-arg "BUILD_VERSION=$BUILD_VERSION" \
    --build-arg "BUILD_TIME=$BUILD_TIME" \
    .

ls -la deploy/dyalog-docs.db
echo "build-db-docker: done. Bake into the web image with deploy/build-images.sh."
