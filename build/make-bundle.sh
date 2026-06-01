#!/usr/bin/env bash
# Wrap dist/whatskept into a proper macOS .app bundle so that:
#
#   - aerospace / Dock / Finder / Spotlight see a real
#     CFBundleIdentifier instead of "NULL-APP-BUNDLE-ID".
#   - The window manager / Mission Control can group windows under
#     the right app name.
#   - Future Developer-ID signing + notarization works (only .app
#     and .dmg can be stapled, plain Mach-Os can't).
#
# Inputs:
#   $1   path to the already-built bare CLI binary (typically
#        dist/whatskept). The bundle is written next to it as
#        WhatsKept.app.
#   $VERSION (env)  baked into CFBundleShortVersionString /
#                   CFBundleVersion. Same value the Makefile passed
#                   via -ldflags so the in-app version, the OS
#                   "Get Info" dialog, and the binary's
#                   `--version` agree.
#
# Output:
#   <dir-of-input>/WhatsKept.app/   (replaces any existing copy)
#
# The bundled binary is the same executable as the CLI \xe2\x80\x94 main()
# detects launch-as-bundle by looking at os.Args[0] and defaults
# to the GUI subcommand in that case (see cmd/whatskept/main.go).

set -euo pipefail

if [[ $# -ne 1 ]]; then
  echo "usage: $0 <path-to-binary>" >&2
  exit 2
fi

BIN="$1"
if [[ ! -x "$BIN" ]]; then
  echo "$0: $BIN is not executable" >&2
  exit 1
fi

VERSION="${VERSION:-0.0.0-dev}"

DIST_DIR="$(dirname "$BIN")"
APP="$DIST_DIR/WhatsKept.app"
APP_MACOS="$APP/Contents/MacOS"
APP_RES="$APP/Contents/Resources"
APP_PLIST="$APP/Contents/Info.plist"

# Always start fresh \xe2\x80\x94 stale signatures + leftover Resources from
# previous runs would otherwise produce a sealed bundle whose hash
# doesn't match its contents.
rm -rf "$APP"
mkdir -p "$APP_MACOS" "$APP_RES"

# Bundled executable. Filename matches CFBundleExecutable below; the
# launch path becomes
# WhatsKept.app/Contents/MacOS/whatskept, which the bundle-detection
# in main.go checks for.
cp "$BIN" "$APP_MACOS/whatskept"
chmod +x "$APP_MACOS/whatskept"

# App icon. Build a multi-resolution .icns from the single source PNG
# (the same file the binary embeds for its runtime Dock icon) so the
# bundle shows the brand icon in Finder / Dock / Spotlight even before
# launch. iconutil wants a .iconset dir of named, sips-scaled PNGs.
REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
ICON_SRC="$REPO_ROOT/internal/app/assets/appicon.png"
HAS_ICON=0
if [[ -f "$ICON_SRC" ]]; then
  ICONSET_PARENT="$(mktemp -d)"
  ICONSET="$ICONSET_PARENT/AppIcon.iconset"
  mkdir -p "$ICONSET"
  for pair in "16 16x16" "32 16x16@2x" "32 32x32" "64 32x32@2x" \
              "128 128x128" "256 128x128@2x" "256 256x256" "512 256x256@2x" \
              "512 512x512" "1024 512x512@2x"; do
    px="${pair% *}"; name="${pair#* }"
    sips -z "$px" "$px" "$ICON_SRC" --out "$ICONSET/icon_$name.png" >/dev/null
  done
  iconutil -c icns "$ICONSET" -o "$APP_RES/appicon.icns"
  rm -rf "$ICONSET_PARENT"
  HAS_ICON=1
else
  echo "$0: warning: $ICON_SRC not found; bundling without an icon" >&2
fi

# Info.plist. Hand-written here rather than checked in as a static
# file so VERSION can be substituted on each build without a
# `sed` dance, and so any plist tweak shows up in `git diff` of this
# script (loud and reviewable) instead of a binary plist.
ICON_KEY=""
if [[ "$HAS_ICON" -eq 1 ]]; then
  ICON_KEY="  <key>CFBundleIconFile</key>            <string>appicon</string>"
fi
cat > "$APP_PLIST" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>CFBundleName</key>                  <string>WhatsKept</string>
  <key>CFBundleDisplayName</key>           <string>WhatsKept</string>
  <key>CFBundleIdentifier</key>            <string>com.whatskept.app</string>
  <key>CFBundleExecutable</key>            <string>whatskept</string>
$ICON_KEY
  <key>CFBundlePackageType</key>           <string>APPL</string>
  <key>CFBundleVersion</key>               <string>$VERSION</string>
  <key>CFBundleShortVersionString</key>    <string>$VERSION</string>
  <key>CFBundleInfoDictionaryVersion</key> <string>6.0</string>
  <key>CFBundleSupportedPlatforms</key>    <array><string>MacOSX</string></array>
  <key>LSMinimumSystemVersion</key>        <string>13.0</string>
  <key>LSApplicationCategoryType</key>     <string>public.app-category.utilities</string>
  <key>NSHighResolutionCapable</key>       <true/>
  <key>NSHumanReadableCopyright</key>      <string>WhatsKept</string>
</dict>
</plist>
PLIST

# Re-sign the inner binary first, then the bundle. Order matters:
# `codesign --deep` against the bundle would also re-sign the inner
# binary, but it's deprecated and noisier than just doing it in two
# explicit passes.
codesign --force --sign - "$APP_MACOS/whatskept"
codesign --force --sign - "$APP"

# Sanity: verify the bundle has a real CFBundleIdentifier.
ID=$(/usr/libexec/PlistBuddy -c "Print :CFBundleIdentifier" "$APP_PLIST")
if [[ -z "$ID" ]]; then
  echo "$0: failed to write CFBundleIdentifier" >&2
  exit 1
fi
echo "bundle: $APP  (id=$ID, version=$VERSION)"
