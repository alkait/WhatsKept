#!/usr/bin/env bash
# WhatsKept curl|bash installer.
#
# Usage:
#   /bin/bash -c "$(curl -fsSL https://github.com/alkait/WhatsKept/releases/latest/download/install.sh)"
#
# Why this exists:
#   WhatsKept is open-source and ad-hoc signed (no Apple Developer
#   account, which costs $99/year). When Safari/Chrome/Firefox
#   download a file, they attach the `com.apple.quarantine` extended
#   attribute. On macOS Sequoia the kernel refuses to launch
#   ad-hoc-signed apps that carry that xattr, with no right-click →
#   Open bypass — users see the "WhatsKept is damaged and can't be
#   opened. You should move it to the Trash." dialog and give up.
#
#   `curl`, by contrast, does not attach com.apple.quarantine to
#   files it writes. So this script — itself running because it was
#   curl'd into bash — can curl down the release zip, extract it,
#   and drop a quarantine-free .app into /Applications, sidestepping
#   Gatekeeper entirely.
#
# What it does:
#   1. Sanity-checks: macOS, arm64, /Applications writable.
#   2. Resolves the latest release tag once up front so all asset
#      downloads are pinned to the same release — defends against
#      the brief CDN window after a publish where
#      `releases/latest/download/<asset>` can hand out different
#      assets from two adjacent releases. Override with
#      $WHATSKEPT_ZIP_URL to pin a specific release or a dev
#      pre-release.
#   3. Downloads WhatsKept-darwin-arm64.app.zip into a temp dir.
#   4. Verifies the SHA-256 against the SHA256SUMS file shipped with
#      the same release, so a man-in-the-middle (or a corrupted
#      mirror) can't slip a tampered binary past us. Skipped only if
#      the user explicitly opts out via $WHATSKEPT_SKIP_VERIFY=1.
#   5. Extracts the zip into the temp dir.
#   6. Quits a running WhatsKept (so we don't fight an open file
#      descriptor on the binary).
#   7. Removes any prior /Applications/WhatsKept.app and ditto's the
#      new one in.
#   8. Strips com.apple.quarantine from the destination — defensive,
#      curl-downloaded files shouldn't have it but `unzip` may copy
#      xattrs forward from inside the zip.
#   9. Resets any stale Full Disk Access TCC entry for our bundle ID
#      so the next backup-list call re-prompts cleanly. Without
#      this, ad-hoc cdhash drift between releases would silently
#      void the previous FDA grant.
#  10. Launches the app.
#
# Idempotent. Safe to re-run after every update.

set -euo pipefail

REPO="alkait/WhatsKept"
DEST="/Applications/WhatsKept.app"
BUNDLE_ID="com.whatskept.app"
# ZIP_URL / SUMS_URL are populated below after we resolve the latest
# release tag, unless the user has overridden them via env vars.

# ---- tiny output helpers -------------------------------------------------

# Detect whether stdout is a TTY; if not (e.g. logged to a file),
# skip ANSI escapes so the output stays readable.
if [[ -t 1 ]]; then
  _BOLD=$'\033[1m'; _DIM=$'\033[2m'; _RED=$'\033[0;31m'
  _GREEN=$'\033[0;32m'; _YELLOW=$'\033[0;33m'; _RESET=$'\033[0m'
else
  _BOLD=''; _DIM=''; _RED=''; _GREEN=''; _YELLOW=''; _RESET=''
fi

step() { printf '%s==>%s %s\n' "$_BOLD" "$_RESET" "$*"; }
ok()   { printf '%s✓%s %s\n'   "$_GREEN" "$_RESET" "$*"; }
warn() { printf '%s!%s %s\n'   "$_YELLOW" "$_RESET" "$*"; }
die()  { printf '%serror:%s %s\n' "$_RED" "$_RESET" "$*" >&2; exit 1; }

# ---- sanity checks -------------------------------------------------------

[[ "$(uname -s)" == "Darwin" ]] || die "this installer is macOS-only (got $(uname -s))"
arch="$(uname -m)"
[[ "$arch" == "arm64" ]] || die "WhatsKept currently ships arm64-only; detected $arch. Build from source per the README."

# /Applications is writable by admins without sudo on a default macOS
# install. If it isn't (corporate-managed Mac, weird perms), bail
# with a clear message rather than spamming sudo prompts.
if [[ ! -w /Applications ]]; then
  die "/Applications is not writable by the current user. Re-run with admin privileges or move WhatsKept.app there manually after extracting."
fi

# ---- temp dir + cleanup --------------------------------------------------

TMP="$(mktemp -d -t whatskept-install)"
trap 'rm -rf "$TMP"' EXIT

ZIP_PATH="$TMP/WhatsKept.app.zip"
SUMS_PATH="$TMP/SHA256SUMS"

# ---- resolve latest release tag -----------------------------------------

# Two reasons we resolve a single tag up front instead of using
# /releases/latest/download/<asset> for each download:
#
#   1. Per-asset CDN race: each `/latest/download/<file>` is a
#      separate redirect, and during the window after a publish
#      different assets can briefly resolve to different releases —
#      producing a SHA-256 mismatch even though each release on its
#      own is consistent.
#   2. Web-redirector staleness: github.com/<repo>/releases/latest
#      and /latest/download/<file> share an internal cache that
#      lags the actual "latest" by minutes after a publish. The
#      GitHub *API* (api.github.com/.../releases/latest) updates
#      instantly, so we go through that instead.
#
# Anonymous API calls are rate-limited to 60/hour per IP, which is
# orders of magnitude more than this script needs. We parse JSON
# with awk so jq isn't required on the user's machine — there's
# exactly one "tag_name" field at the top of the response and we
# stop after the first match.
if [[ -n "${WHATSKEPT_ZIP_URL:-}" ]]; then
  # Caller pinned a specific URL — honour it and skip tag resolution.
  ZIP_URL="$WHATSKEPT_ZIP_URL"
  SUMS_URL="${WHATSKEPT_SUMS_URL:-${ZIP_URL%/*}/SHA256SUMS}"
  TAG="(pinned)"
else
  step "Resolving latest release"
  TAG="$(curl -fsL --retry 2 \
    -H 'Accept: application/vnd.github+json' \
    "https://api.github.com/repos/${REPO}/releases/latest" \
    | awk -F'"' '/"tag_name":/ {print $4; exit}')" \
    || die "could not reach api.github.com to resolve latest release"
  if [[ -z "$TAG" || "$TAG" == "latest" ]]; then
    die "could not parse latest release tag from GitHub API response"
  fi
  BASE="https://github.com/${REPO}/releases/download/${TAG}"
  ZIP_URL="$BASE/WhatsKept-darwin-arm64.app.zip"
  SUMS_URL="$BASE/SHA256SUMS"
  ok "latest release: $TAG"
fi

# ---- download ------------------------------------------------------------

step "Downloading WhatsKept from $ZIP_URL"
# -L follow redirects (GitHub releases redirect to S3).
# -f fail on HTTP errors so a 404 doesn't write an HTML error page.
# -S show errors when -s is set; -s suppress progress meter.
# --retry survive a flaky network without making the user re-run.
curl -fL --retry 3 --retry-delay 2 -o "$ZIP_PATH" "$ZIP_URL" \
  || die "failed to download $ZIP_URL"

# ---- verify checksum -----------------------------------------------------

if [[ "${WHATSKEPT_SKIP_VERIFY:-0}" == "1" ]]; then
  warn "skipping checksum verification (WHATSKEPT_SKIP_VERIFY=1)"
else
  step "Verifying SHA-256 against $SUMS_URL"
  if ! curl -fsL --retry 2 -o "$SUMS_PATH" "$SUMS_URL"; then
    die "failed to download SHA256SUMS — re-run with WHATSKEPT_SKIP_VERIFY=1 to bypass (not recommended)"
  fi
  # The SHA256SUMS file lists multiple assets keyed by basename. Pull
  # out the line for our specific zip and feed it to shasum -c so we
  # don't have to parse the format ourselves.
  basename_zip="$(basename "$ZIP_URL")"
  expected_line="$(grep -E "[[:space:]]${basename_zip}\$" "$SUMS_PATH" || true)"
  if [[ -z "$expected_line" ]]; then
    die "no checksum entry for $basename_zip in SHA256SUMS — release may be corrupt"
  fi
  # shasum -c expects the path as it appears in the sums file,
  # relative to the cwd of the shasum invocation. Easiest: stage a
  # symlink with the right name and run shasum in that dir.
  ln -s "$ZIP_PATH" "$TMP/$basename_zip"
  ( cd "$TMP" && printf '%s\n' "$expected_line" | shasum -a 256 -c --status ) \
    || die "SHA-256 mismatch for $basename_zip — refusing to install a corrupted download"
  ok "checksum matches"
fi

# ---- extract -------------------------------------------------------------

step "Extracting"
# -q quiet; -d output dir.
unzip -q "$ZIP_PATH" -d "$TMP/extracted"

# The release zip stages everything inside a versioned folder
# (WhatsKept-darwin-arm64-<ver>/), so search for WhatsKept.app
# anywhere under the extraction root instead of hard-coding the
# folder name and breaking on the next version bump.
SRC_APP="$(find "$TMP/extracted" -maxdepth 3 -name WhatsKept.app -type d -print -quit)"
[[ -n "$SRC_APP" ]] || die "WhatsKept.app not found inside the downloaded zip — release may be malformed"

# ---- quit running instance ----------------------------------------------

if pgrep -f "/Applications/WhatsKept.app/Contents/MacOS/whatskept" >/dev/null 2>&1; then
  step "Quitting running WhatsKept"
  osascript -e 'tell application "WhatsKept" to quit' 2>/dev/null || true
  # Give LaunchServices a moment to deliver the quit + tear down the
  # process, otherwise the rm below races a held file descriptor.
  sleep 1
fi

# ---- install -------------------------------------------------------------

step "Installing to $DEST"
rm -rf "$DEST"
# ditto preserves resource forks, codesign metadata, and symlinks
# inside the bundle. Plain `cp -R` (or `mv`, which falls back to
# copy across filesystems) can subtly corrupt the bundle.
ditto "$SRC_APP" "$DEST"

# Defensive xattr strip. curl-downloaded files don't carry
# com.apple.quarantine, but the unzip step may have preserved one
# baked into the zip itself by the GitHub release pipeline. Removing
# it here means the kernel sees the .app as locally-installed
# software, not a downloaded artifact.
xattr -dr com.apple.quarantine "$DEST" 2>/dev/null || true

# ---- reset stale FDA grant ----------------------------------------------

# TCC anchors ad-hoc Full Disk Access grants on the per-build cdhash,
# which changes on every release. Without this reset, an updated
# install silently fails to read iOS backups even though the FDA
# toggle still appears ON. tccutil exits non-zero if there's no entry
# to reset (first-time install) — that's fine.
step "Clearing any stale Full Disk Access grant"
tccutil reset SystemPolicyAllFiles "$BUNDLE_ID" 2>/dev/null || true

# ---- open FDA pane + reveal app in Finder -------------------------------

# After tccutil reset the user has to manually re-grant Full Disk
# Access — Apple deliberately doesn't expose a `tccutil grant` and
# the TCC database is SIP-protected, so this is unfixable without a
# real Developer ID + notarization. The least we can do is put both
# windows the user needs right in front of them:
#
#   1. System Settings deep-linked to the Full Disk Access pane via
#      the documented x-apple.systempreferences URL scheme. Lands
#      directly on the right list, no menu navigation.
#   2. Finder revealing /Applications/WhatsKept.app with the bundle
#      pre-selected. System Settings accepts apps via drag-drop into
#      the FDA list, so the user drags from Finder onto the pane and
#      is done — no clicking +, no navigating to /Applications.
#
# The two `open` calls are non-blocking; both windows surface and
# the script keeps moving.
step "Opening Full Disk Access pane and revealing WhatsKept in Finder"
open "x-apple.systempreferences:com.apple.preference.security?Privacy_AllFiles" 2>/dev/null || true
open -R "$DEST" 2>/dev/null || true

# ---- launch --------------------------------------------------------------

# Launch the app last so it ends up the foreground window — it's the
# thing the user actually came to see. Once they grant FDA via the
# pane we just opened, the app needs to be quit + reopened for the
# new TCC grant to take effect on the running process; we tell them
# that in the post-install banner below.
step "Launching WhatsKept"
open "$DEST"

echo
ok "WhatsKept installed at $DEST"
echo
printf '%sGrant Full Disk Access:%s\n' "$_BOLD" "$_RESET"
echo "  Two windows just opened for you:"
echo "    • System Settings → Privacy & Security → Full Disk Access"
echo "    • Finder, with WhatsKept.app pre-selected in /Applications"
echo
echo "  Drag WhatsKept.app from the Finder window into the Full Disk"
echo "  Access list, toggle it ON, then quit and reopen WhatsKept."
echo "  (The grant doesn't take effect on an already-running process.)"
echo
echo "  WhatsKept reads iOS backups from"
echo "    ~/Library/Application Support/MobileSync/Backup"
echo "  which macOS protects behind FDA — there is no way to skip"
echo "  this step without an Apple Developer ID."
echo
printf '%sUpdates:%s re-run this installer. macOS attaches a fresh\n' "$_DIM" "$_RESET"
printf '%s         %s cdhash to every release, which voids the previous FDA\n' "$_DIM" "$_RESET"
printf '%s         %s grant — the installer resets the stale entry for you.\n' "$_DIM" "$_RESET"
