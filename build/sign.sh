#!/usr/bin/env bash
# Re-sign a Go-built Mach-O after `go build`.
#
# On Apple Silicon, Go's linker produces an ad-hoc + linker-signed binary,
# but post-link byte modifications by macOS provenance/security tooling can
# silently invalidate the signature -- producing a binary the kernel will
# refuse to start (process freezes in `U` state with 0 CPU).
#
# Re-signing with `codesign --force --sign -` regenerates the signature
# over the current bytes, which fixes the problem unconditionally.
set -euo pipefail

if [[ $# -lt 1 ]]; then
  echo "usage: $0 <binary> [<binary> ...]" >&2
  exit 2
fi

for f in "$@"; do
  if [[ ! -f "$f" ]]; then
    echo "skip: $f (not a file)" >&2
    continue
  fi
  codesign --force --sign - "$f"
  echo "signed: $f"
done
