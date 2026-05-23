#!/usr/bin/env bash
# Build the embedded helpers bundle from a local Homebrew install of
# libimobiledevice. Output lands in internal/helpers/bundle/ where it
# is picked up by go:embed at compile time.
#
# This script is run by maintainers (or CI), NOT by end users. End users
# get the already-bundled binaries baked into the Go binary.
#
# Strategy:
#   1. Discover idevicebackup2 + idevice_id on PATH.
#   2. Recursively collect their non-system dylib dependencies.
#   3. Copy the lot to bundle/ as flat files.
#   4. Rewrite every LC_LOAD_DYLIB reference to /opt/homebrew/* (or
#      /usr/local/* on Intel) to @loader_path/<basename>. Same for the
#      LC_ID_DYLIB of each .dylib.
#   5. Re-sign each binary/dylib so macOS will load them.
#
# Result: a flat directory of self-contained, relocatable Mach-Os that
# work no matter where they are unpacked.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
DEST="$PROJECT_ROOT/internal/helpers/bundle"

# Tools the GUI needs to invoke directly. Add more if/when the app
# grows additional libimobiledevice features.
TOOLS=(idevicebackup2 idevice_id)

# Resolve symlinks (macOS readlink lacks -f, use python).
resolve() { python3 -c 'import os,sys; print(os.path.realpath(sys.argv[1]))' "$1"; }

# Is this dylib path one we should skip (system / Apple-shipped)?
is_system() {
  case "$1" in
    /System/*|/usr/lib/*) return 0 ;;
    *)                    return 1 ;;
  esac
}

mkdir -p "$DEST"
rm -rf "$DEST"/*

WORK=$(mktemp); DONE=$(mktemp); trap "rm -f $WORK $DONE" EXIT

# Seed worklist with each tool's resolved binary path.
for t in "${TOOLS[@]}"; do
  bin=$(command -v "$t" 2>/dev/null || true)
  [[ -z "$bin" ]] && { echo "ERROR: $t not found on PATH (brew install libimobiledevice)" >&2; exit 1; }
  resolve "$bin" >> "$WORK"
done

# Iterate, expanding each item's non-system deps into the worklist.
while [[ -s "$WORK" ]]; do
  item=$(head -n1 "$WORK")
  tail -n +2 "$WORK" > "$WORK.tmp" && mv "$WORK.tmp" "$WORK"
  grep -qxF "$item" "$DONE" 2>/dev/null && continue
  echo "$item" >> "$DONE"

  while IFS= read -r dep; do
    [[ -z "$dep" ]] && continue
    is_system "$dep" && continue
    real=$(resolve "$dep")
    # Skip self-reference (LC_ID_DYLIB for dylibs).
    [[ "$real" == "$item" ]] && continue
    grep -qxF "$real" "$DONE" 2>/dev/null && continue
    echo "$real" >> "$WORK"
  done < <(otool -L "$item" 2>/dev/null | tail -n +2 | awk '{print $1}')
done

# Copy each item to DEST as a flat file, writable.
echo "[1/3] copying $(wc -l < "$DONE" | tr -d ' ') files to $DEST"
while IFS= read -r src; do
  base=$(basename "$src")
  cp "$src" "$DEST/$base"
  chmod u+w "$DEST/$base"
done < "$DONE"

# Rewrite install names to @loader_path.
echo "[2/3] rewriting install names"
for f in "$DEST"/*; do
  base=$(basename "$f")

  # Set this dylib's own LC_ID_DYLIB to a relocatable path.
  if [[ "$base" == *.dylib ]]; then
    install_name_tool -id "@loader_path/$base" "$f"
  fi

  # Rewrite each non-system LC_LOAD_DYLIB entry.
  while IFS= read -r dep; do
    [[ -z "$dep" ]] && continue
    is_system "$dep" && continue

    # Resolve to find the concrete file we copied.
    real=$(resolve "$dep")
    realbase=$(basename "$real")

    # Skip self-reference.
    [[ "$realbase" == "$base" ]] && continue

    install_name_tool -change "$dep" "@loader_path/$realbase" "$f" 2>&1 \
      | grep -v "no LC_LOAD_DYLIB" || true
  done < <(otool -L "$f" 2>/dev/null | tail -n +2 | awk '{print $1}')
done

# Re-sign. install_name_tool invalidates the existing signature.
echo "[3/3] re-signing"
for f in "$DEST"/*; do
  codesign --force --sign - "$f" 2>&1 | grep -v "replacing existing signature" || true
done

echo
echo "bundle ready: $DEST"
ls -lh "$DEST"
