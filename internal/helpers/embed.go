// Package helpers ships and runs the libimobiledevice command-line
// tools (idevicebackup2, idevice_id) that the whatskept GUI needs to
// drive iOS backups, plus all the Mach-O dylibs they depend on.
//
// The whole bundle is baked into the Go binary at compile time via
// go:embed. On first run it is extracted to a cache directory under
// ~/Library/Caches/whatskept/bin/ and Path() returns that directory so
// callers can prepend it to PATH for subprocess invocations. A SHA-256
// content hash is written alongside the cache so subsequent launches
// can fast-path when the embedded bundle is unchanged, and re-extract
// automatically when a new whatskept release ships an updated bundle.
//
// The tools are signed and have all their LC_LOAD_DYLIB / LC_ID_DYLIB
// references rewritten to @loader_path so they load correctly from
// any cache directory location. See build/bundle-helpers.sh.
package helpers

import "embed"

//go:embed bundle/*
var bundleFS embed.FS
