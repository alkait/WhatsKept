# whatskept

Searchable, agent-queryable WhatsApp history from an iOS backup, in Go.

A single self-contained ~17 MB binary. No Homebrew at runtime, no
internet, no Python. Drives iOS backups, decrypts WhatsApp's
ChatStorage.sqlite, and (eventually) feeds it into a searchable
SQLite + FTS5 workspace that an agent can query directly.

This is the Go rewrite of the original Python implementation at
`~/CascadeProjects/wa-extract/`. The Python version remains the source
of truth until this rewrite reaches feature parity.

## Build

```bash
make build            # → dist/whatskept (always re-signs after build)
```

The Makefile invokes `build/sign.sh` after `go build`. This is **not
optional** on Apple Silicon: post-link byte modifications by macOS
tooling can silently invalidate the linker-emitted ad-hoc signature,
producing a binary the kernel refuses to start (the process freezes
with zero CPU time and cannot be killed). Re-signing fixes it
unconditionally.

First build is slow (~60 s) because `mattn/go-sqlite3` is a transitive
cgo dep that has to compile SQLite from C. Subsequent builds are
sub-second.

## Run

```bash
./dist/whatskept --help
./dist/whatskept list                  # discovered iOS backups
./dist/whatskept extract               # decrypt WhatsApp DB → ./ChatStorage.sqlite
./dist/whatskept extract -o ~/Documents/my-whatsapp-chat
./dist/whatskept app                   # native macOS GUI (Backup tab)
```

Or via Make:

```bash
make list
make extract
make app
```

The `extract` subcommand reads the backup password from
`$BACKUP_PASSWORD` or a `.env` file in the output directory (or any
parent). Never prompts.

The `app` subcommand opens a native WKWebView window; closing the
window terminates the embedded HTTP server. On first launch macOS will
ask for **Full Disk Access** — grant it under System Settings → Privacy
& Security → Full Disk Access so the app can read
`~/Library/Application Support/MobileSync/Backup/`.

## What's embedded

The binary is genuinely standalone — everything required at runtime is
baked in via `go:embed`:

- `idevicebackup2`, `idevice_id`, plus `libimobiledevice`,
  `libimobiledevice-glue`, `libplist`, `libusbmuxd`, `libssl`,
  `libcrypto` (~6 MB). All install names rewritten to `@loader_path`
  by `build/bundle-helpers.sh` so the bundle is fully relocatable.
  Extracted to `~/Library/Caches/whatskept/bin/` on first run; auto-
  re-extracts when a new release ships an updated bundle.
- React 18, ReactDOM 18, Babel-standalone, Tailwind Play CDN
  (~3.3 MB). Vendored under `internal/app/web/vendor/`. The GUI
  doesn't contact unpkg.com or cdn.tailwindcss.com on launch.
- The single `index.html` React UI (`internal/app/web/index.html`).

## Layout

```
whatskept/
├── cmd/whatskept/             main binary (cobra subcommands)
│   ├── main.go
│   ├── list.go
│   ├── extract.go
│   └── app.go                 GUI entry point
├── internal/
│   ├── backup/                iOS backup discovery + decryption
│   │                            wraps github.com/dunhamsteve/ios
│   ├── secrets/               BACKUP_PASSWORD resolution (env + .env)
│   ├── helpers/               embedded idevicebackup2 + dylibs
│   │   ├── embed.go
│   │   ├── extract.go         cache-extract + PATH injection
│   │   └── bundle/            self-contained, relocatable Mach-Os
│   └── app/                   GUI server, window, jobs, workspace
│       ├── server.go          stdlib HTTP routes (workspace, devices,
│       │                        backups, jobs, SSE)
│       ├── window.go          webview_go (WKWebView) bootstrap
│       ├── jobs.go            in-process job manager + SSE pump +
│       │                        orphan-process adoption
│       ├── workspace.go       active workspace + recent persistence
│       ├── dialog_darwin.go   native folder picker (osascript)
│       └── web/
│           ├── index.html     React UI (Babel-in-browser)
│           └── vendor/        pinned React/ReactDOM/Babel/Tailwind
├── build/
│   ├── sign.sh                post-build codesign step
│   └── bundle-helpers.sh      rebuild internal/helpers/bundle/
├── Makefile
└── README.md (this file)
```

## Subcommand status

| Subcommand                | Python (wa-extract)       | Go (here)         |
| ------------------------- | ------------------------- | ----------------- |
| `whatskept list`          | parts of `cli.py`         | done              |
| `whatskept extract`       | `cli.py` + `extract.py`   | done (decrypt)    |
| `whatskept app`           | `app/main.py`             | done (Backup tab) |
| `whatskept media-index`   | `media_indexer.py`        | pending           |
| `whatskept voice-index`   | `voice_indexer.py`        | pending           |
| `whatskept contacts-sync` | `contacts_sync.py`        | pending           |
| `whatskept profiles-sync` | `profiles_sync.py`        | pending           |
| views.sql + FTS rebuild   | `postprocess.py`          | pending           |
| AGENTS.md scaffold        | `postprocess.py`          | pending           |

The `app` subcommand currently exposes the **Backup tab only**: list
existing iOS backups, delete a backup, drive a fresh backup over USB
with live progress streaming via Server-Sent Events. The Database tab
is hidden until `postprocess` is ported.

## Validated foundations

Two feasibility spikes were run before the rewrite started; both
passed against a real iPhone 15 on iOS 26.3.1, then deleted:

- `idevicebackup2` + libimobiledevice dylibs embedded in a Go binary,
  extracted to a cache on first run, drives a fresh iOS backup over
  USB with zero runtime Homebrew dependency.
- `github.com/dunhamsteve/ios` decrypts a 280 MB encrypted
  `ChatStorage.sqlite` end-to-end in ~5.5 s with row counts matching
  the Python pipeline exactly.

Both proven techniques are now wired into the production codebase
(see `internal/helpers/` and `internal/backup/extract.go`).
