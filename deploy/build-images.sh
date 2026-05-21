#!/usr/bin/env bash
#
# Build the docsearch-web and docsearch-embedder OCI images via
# buildx. Defaults to linux/arm64 (Apple Silicon hosts); override
# with PLATFORMS for multi-arch builds.
#
# Usage:
#   ./build-images.sh                # local load, linux/arm64, tag=latest
#   TAG=2026-05-21 ./build-images.sh # tagged build
#   PUSH=1 REGISTRY=ghcr.io/xpqz \
#       TAG=2026-05-21 ./build-images.sh
#
# Requires deploy/dyalog-docs.db (run ./build-db.sh first).

set -euo pipefail

cd "$(dirname "$0")"

if [ ! -f dyalog-docs.db ]; then
  echo "build-images: dyalog-docs.db not found; run ./build-db.sh first" >&2
  exit 1
fi

PLATFORMS="${PLATFORMS:-linux/arm64}"
TAG="${TAG:-latest}"
REGISTRY="${REGISTRY:-localhost}"
PUSH="${PUSH:-0}"

WEB_IMAGE="${REGISTRY}/docsearch-web:${TAG}"
EMB_IMAGE="${REGISTRY}/docsearch-embedder:${TAG}"

out_flag="--load"
if [ "$PUSH" = "1" ]; then
  out_flag="--push"
fi

echo "build-images: web -> ${WEB_IMAGE} (${PLATFORMS})"
docker buildx build \
    --platform "$PLATFORMS" \
    -f Dockerfile.web \
    -t "$WEB_IMAGE" \
    $out_flag \
    ..

echo "build-images: embedder -> ${EMB_IMAGE} (${PLATFORMS})"
docker buildx build \
    --platform "$PLATFORMS" \
    -f Dockerfile.embedder \
    -t "$EMB_IMAGE" \
    $out_flag \
    ..

echo "build-images: done. Bring the stack up with:"
echo "    REGISTRY=$REGISTRY TAG=$TAG docker compose up -d"
