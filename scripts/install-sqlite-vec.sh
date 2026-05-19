#!/usr/bin/env sh
# Download the sqlite-vec loadable extension for the host platform
# into $INSTALL_DIR (default: ~/.bundle-docs), verify it via the
# release checksums.txt, and print the installed path on stdout so
# it can be piped to docsearch -vector-extension "$(...)".
#
# Usage:
#   scripts/install-sqlite-vec.sh
#   SQLITE_VEC_VERSION=0.1.9 INSTALL_DIR=/opt scripts/install-sqlite-vec.sh

set -eu

VERSION="${SQLITE_VEC_VERSION:-0.1.9}"
INSTALL_DIR="${INSTALL_DIR:-$HOME/.bundle-docs}"
RELEASE_BASE="https://github.com/asg017/sqlite-vec/releases/download/v${VERSION}"

OS=$(uname -s)
ARCH=$(uname -m)

case "$OS" in
  Darwin) PLATFORM_OS=macos ;;
  Linux)  PLATFORM_OS=linux ;;
  MINGW*|MSYS*|CYGWIN*) PLATFORM_OS=windows ;;
  *)
    echo "install-sqlite-vec: unsupported OS '$OS'" >&2
    exit 1
    ;;
esac

case "$ARCH" in
  arm64|aarch64) PLATFORM_ARCH=aarch64 ;;
  x86_64|amd64)  PLATFORM_ARCH=x86_64 ;;
  *)
    echo "install-sqlite-vec: unsupported arch '$ARCH'" >&2
    exit 1
    ;;
esac

case "$PLATFORM_OS" in
  macos)   EXT_FILE=vec0.dylib ;;
  linux)   EXT_FILE=vec0.so ;;
  windows) EXT_FILE=vec0.dll ;;
esac

ASSET="sqlite-vec-${VERSION}-loadable-${PLATFORM_OS}-${PLATFORM_ARCH}.tar.gz"
URL="${RELEASE_BASE}/${ASSET}"
CHECKSUMS_URL="${RELEASE_BASE}/checksums.txt"

mkdir -p "$INSTALL_DIR"
TMPDIR=$(mktemp -d)
trap 'rm -rf "$TMPDIR"' EXIT

echo "install-sqlite-vec: downloading $URL" >&2
curl -fsSL --retry 3 -o "$TMPDIR/asset.tar.gz" "$URL"

echo "install-sqlite-vec: verifying checksum" >&2
curl -fsSL --retry 3 -o "$TMPDIR/checksums.txt" "$CHECKSUMS_URL"
EXPECTED=$(awk -v name="$ASSET" '$1 == name { print $2 } $2 == name { print $1 }' "$TMPDIR/checksums.txt" | head -n1)
if [ -z "$EXPECTED" ]; then
  echo "install-sqlite-vec: no checksum entry for $ASSET" >&2
  exit 1
fi
if command -v shasum >/dev/null 2>&1; then
  ACTUAL=$(shasum -a 256 "$TMPDIR/asset.tar.gz" | awk '{print $1}')
elif command -v sha256sum >/dev/null 2>&1; then
  ACTUAL=$(sha256sum "$TMPDIR/asset.tar.gz" | awk '{print $1}')
else
  echo "install-sqlite-vec: neither shasum nor sha256sum found; cannot verify download" >&2
  exit 1
fi
if [ "$EXPECTED" != "$ACTUAL" ]; then
  echo "install-sqlite-vec: checksum mismatch for $ASSET" >&2
  echo "  expected: $EXPECTED" >&2
  echo "  actual:   $ACTUAL" >&2
  exit 1
fi

tar -xzf "$TMPDIR/asset.tar.gz" -C "$TMPDIR"
if [ ! -f "$TMPDIR/$EXT_FILE" ]; then
  echo "install-sqlite-vec: $EXT_FILE not found inside $ASSET" >&2
  ls -la "$TMPDIR" >&2
  exit 1
fi

INSTALL_PATH="$INSTALL_DIR/$EXT_FILE"
mv "$TMPDIR/$EXT_FILE" "$INSTALL_PATH"
chmod 0644 "$INSTALL_PATH"

# Best-effort version probe. macOS's system sqlite3 is built without
# extension-loading support, so a failure here is informational only.
if command -v sqlite3 >/dev/null 2>&1; then
  if version=$(sqlite3 :memory: ".load $INSTALL_PATH" "select vec_version();" 2>/dev/null); then
    echo "install-sqlite-vec: installed sqlite-vec $version" >&2
  else
    echo "install-sqlite-vec: extension installed at $INSTALL_PATH (sqlite3 CLI cannot load extensions; this is expected on macOS system sqlite3)" >&2
  fi
fi

printf '%s\n' "$INSTALL_PATH"
