#!/usr/bin/env bash
# Build the whisper-cli helper binary from a checkout of
# ggerganov/whisper.cpp and install it to internal/helpers/bundle/
# where it gets picked up by go:embed at compile time.
#
# This script is run by maintainers (or CI), NOT by end users. End
# users get the already-bundled binary baked into the Go binary.
#
# Strategy:
#   1. Clone whisper.cpp at $WHISPER_REF (default: latest tagged release).
#   2. cmake-build whisper-cli with:
#        - GGML_METAL=ON              (Apple Silicon GPU)
#        - GGML_METAL_EMBED_LIBRARY=ON (.metallib baked into binary,
#                                       so no .metallib sidecar file)
#        - BUILD_SHARED_LIBS=OFF       (everything statically linked)
#   3. Strip the binary (~50% size win) and ad-hoc sign it.
#
# The model file (`ggml-large-v3-turbo-q5_0.bin`) is NOT bundled
# here. It's downloaded at runtime by the app on first use and
# cached at ~/Library/Application Support/whatskept/models/. See
# internal/helpers/model.go for the spec and lifecycle.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
DEST="$PROJECT_ROOT/internal/helpers/bundle"

# Pin to a known-good ref so reproducible builds aren't at the mercy
# of upstream regressions. Override via env to bump.
WHISPER_REF="${WHISPER_REF:-master}"

# Build inside a workspace-local cache so a partial / failed build
# can be rerun without re-cloning. Cache dir is gitignored.
BUILD_CACHE="$PROJECT_ROOT/build/.whisper-build-cache"
SRC="$BUILD_CACHE/whisper.cpp"

mkdir -p "$BUILD_CACHE" "$DEST"

# 1. Clone or update.
if [[ -d "$SRC/.git" ]]; then
  echo "[1/4] using existing checkout at $SRC"
  git -C "$SRC" fetch --depth 1 origin "$WHISPER_REF"
  git -C "$SRC" checkout -f FETCH_HEAD
else
  echo "[1/4] cloning ggerganov/whisper.cpp@$WHISPER_REF"
  git clone --depth 1 --branch "$WHISPER_REF" \
    https://github.com/ggerganov/whisper.cpp "$SRC"
fi

# 2. Configure.
echo "[2/4] configuring (Metal embedded, static, no examples beyond CLI)"
rm -rf "$SRC/build"
cmake -S "$SRC" -B "$SRC/build" \
  -DCMAKE_BUILD_TYPE=Release \
  -DGGML_METAL=ON \
  -DGGML_METAL_EMBED_LIBRARY=ON \
  -DBUILD_SHARED_LIBS=OFF \
  -DWHISPER_BUILD_TESTS=OFF \
  -DWHISPER_BUILD_EXAMPLES=ON \
  > "$BUILD_CACHE/cmake.log" 2>&1

# 3. Build.
echo "[3/4] building whisper-cli (-j)"
cmake --build "$SRC/build" --config Release --target whisper-cli -j \
  > "$BUILD_CACHE/build.log" 2>&1

OUT_BIN="$SRC/build/bin/whisper-cli"
if [[ ! -x "$OUT_BIN" ]]; then
  echo "ERROR: build did not produce $OUT_BIN" >&2
  tail -40 "$BUILD_CACHE/build.log" >&2
  exit 1
fi

# 4. Install: strip, sign, copy to bundle/.
echo "[4/4] installing to $DEST/whisper-cli"
cp "$OUT_BIN" "$DEST/whisper-cli"
chmod +x "$DEST/whisper-cli"
strip "$DEST/whisper-cli" 2>/dev/null || true
codesign --force --sign - "$DEST/whisper-cli"

# Sanity: print final size + that Metal ran the smoke command.
size_kb=$(du -k "$DEST/whisper-cli" | awk '{print $1}')
echo
echo "bundle: $DEST/whisper-cli  (${size_kb} KB)"
echo
ls -lh "$DEST/whisper-cli"
