#!/usr/bin/env bash
# Install WhatsKept.command — double-click to install WhatsKept.app
# into /Applications and launch it.
#
# WhatsKept is open-source and ad-hoc signed (no Apple Developer
# account, which costs $99/year). When you download a zip from
# GitHub, macOS attaches a `com.apple.quarantine` extended attribute
# to every file inside, and Gatekeeper refuses to open ad-hoc-signed
# apps that carry that flag — you get the "WhatsKept is damaged and
# can't be opened. You should move it to the Trash." dialog, with
# no easy bypass on Sequoia (the old right-click → Open trick was
# removed for unsigned apps in macOS 15).
#
# This script is the workaround:
#   1. Quits a running WhatsKept (so the copy doesn't fight an open
#      file descriptor).
#   2. Strips com.apple.quarantine from the bundled WhatsKept.app
#      that lives next to this script.
#   3. ditto-copies it into /Applications, replacing any prior
#      install. ditto preserves codesign metadata that plain `cp`
#      would strip.
#   4. Strips quarantine from the destination too (belt + braces;
#      ditto sometimes copies xattrs forward).
#   5. Resets any stale Full Disk Access TCC entry for our bundle ID.
#      This matters because TCC anchors ad-hoc grants on the per-build
#      `cdhash`, which changes on every release — so even if you
#      previously granted FDA, the toggle would silently stop working
#      after an update. Resetting clears the stale entry so the next
#      backup-list call re-prompts cleanly.
#   6. Launches the app.
#
# Re-run this script after every WhatsKept update. macOS attaches
# fresh quarantine xattrs to every download, and the cdhash drift
# means the previous FDA grant is dead anyway.
#
# Notes:
#   - The .command file itself is also quarantined when downloaded.
#     The first time you double-click, macOS may say "cannot verify
#     the developer of 'Install WhatsKept.command'". If so, click
#     Cancel, right-click the file, choose Open, then confirm Open
#     in the next dialog. (Right-click → Open still works for shell
#     scripts on Sequoia; it's only ad-hoc-signed .app bundles that
#     lost that bypass.)
#   - The script does not need sudo. /Applications is writable by
#     the local admin user.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SRC="$SCRIPT_DIR/WhatsKept.app"
DEST="/Applications/WhatsKept.app"
BUNDLE_ID="com.whatskept.app"

# Tiny ANSI helpers for clearer output. Terminal renders these; if
# anyone pipes the script somewhere weird, the codes are harmless.
b()   { printf '\033[1m%s\033[0m\n'   "$*"; }
ok()  { printf '\033[0;32m%s\033[0m\n' "$*"; }
warn(){ printf '\033[0;33m%s\033[0m\n' "$*"; }
err() { printf '\033[0;31m%s\033[0m\n' "$*"; }

b "Installing WhatsKept…"
echo

if [[ ! -d "$SRC" ]]; then
  err "WhatsKept.app not found next to this installer."
  err "Expected: $SRC"
  echo
  echo "Make sure you're running this from inside the unzipped"
  echo "release folder. The .command file and WhatsKept.app should"
  echo "sit side by side."
  echo
  echo "Press any key to close this window."
  read -r -n 1 -s
  exit 1
fi

# 1. Quit a running instance so we don't fight held file descriptors
#    or get a "resource busy" rename error.
if pgrep -f "/Applications/WhatsKept.app/Contents/MacOS/whatskept" >/dev/null 2>&1; then
  echo "Quitting running WhatsKept…"
  osascript -e 'tell application "WhatsKept" to quit' 2>/dev/null || true
  # Give LaunchServices a moment to deliver the quit + tear down.
  sleep 1
fi

# 2. Strip quarantine on the source. xattr exits non-zero if there
#    is no quarantine attribute to remove (e.g. zip extracted under
#    Terminal `unzip` rather than Archive Utility), so swallow that.
echo "Stripping quarantine attribute…"
xattr -dr com.apple.quarantine "$SRC" 2>/dev/null || true

# 3. Replace any existing install. We rm + ditto rather than `mv`
#    so an existing /Applications/WhatsKept.app from an older
#    release is fully overwritten — `mv` would error on a non-empty
#    target on some filesystems.
echo "Copying to /Applications…"
rm -rf "$DEST"
ditto "$SRC" "$DEST"

# 4. Belt + braces: clear quarantine on the destination too.
xattr -dr com.apple.quarantine "$DEST" 2>/dev/null || true

# 5. Reset any stale Full Disk Access grant. tccutil exits non-zero
#    if there is no entry for the bundle ID, which is fine on a
#    first install — swallow it.
echo "Clearing any stale Full Disk Access grant…"
tccutil reset SystemPolicyAllFiles "$BUNDLE_ID" 2>/dev/null || true

# 6. Launch.
echo "Launching WhatsKept…"
open "$DEST"

echo
ok "Done. WhatsKept is installed at $DEST"
echo
b "First launch:"
echo "  WhatsKept needs Full Disk Access to read iOS backups under"
echo "  ~/Library/Application Support/MobileSync/Backup. When the"
echo "  Backups tab shows the FDA error, click + in System Settings →"
echo "  Privacy & Security → Full Disk Access, choose"
echo "  /Applications/WhatsKept.app, toggle it ON, then quit and"
echo "  reopen WhatsKept."
echo
warn "After every update, re-run this installer. macOS re-quarantines"
warn "the new download, and the per-build cdhash means the previous"
warn "Full Disk Access grant doesn't carry over."
echo
echo "Press any key to close this window."
read -r -n 1 -s
