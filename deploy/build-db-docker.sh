#!/usr/bin/env bash
#
# Reproducible DB build that needs only Docker on the host. Wraps
# `docker buildx build` against deploy/Dockerfile.dbbuilder and uses
# buildx's local output mode to drop the produced dyalog-docs.db
# back onto the host filesystem.
#
# The DB is a plain SQLite file and is byte-identical regardless of
# CPU architecture, so this build is deliberately SINGLE-platform and
# native: it omits --platform and lets buildx use the builder's own
# arch. That matters in two ways:
#   - No emulation. Building the DB for a non-native platform runs the
#     embedding model's PyTorch inference under QEMU, which is so slow
#     the semantic-index step trips its embed-client timeout.
#   - Clean output path. `--output type=local` only writes
#     deploy/dyalog-docs.db directly for a single platform; with
#     multiple platforms it would split into deploy/linux_amd64/...
#     and deploy/linux_arm64/... subdirs instead.
#
# The multi-arch PLATFORMS knob belongs to the runtime image build
# (deploy/build-images.sh), not here; it is intentionally ignored.
#
# Usage:
#   ./deploy/build-db-docker.sh
#   DOCS_REF=abc123 ./deploy/build-db-docker.sh    # pin upstream

set -euo pipefail

cd "$(dirname "$0")/.."

DOCS_REF="${DOCS_REF:-}"
BUILD_VERSION="${BUILD_VERSION:-$(git rev-parse --short HEAD 2>/dev/null || echo dev)}"
BUILD_TIME="${BUILD_TIME:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}"

echo "build-db-docker: native platform, docs_ref=${DOCS_REF:-tip-of-main}"

docker buildx build \
    --target db \
    --output "type=local,dest=deploy" \
    -f deploy/Dockerfile.dbbuilder \
    --build-arg "DOCS_REF=$DOCS_REF" \
    --build-arg "BUILD_VERSION=$BUILD_VERSION" \
    --build-arg "BUILD_TIME=$BUILD_TIME" \
    .

ls -la deploy/dyalog-docs.db
echo "build-db-docker: done. Bake into the web image with deploy/build-images.sh."
