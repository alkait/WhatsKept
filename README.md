# whatskept

Searchable, agent-queryable WhatsApp history from an iOS backup, in Go.

This is the Go rewrite of the original Python implementation at
`~/CascadeProjects/wa-extract/`. The Python version remains the source of
truth and the working tool until this rewrite reaches feature parity, at
which point the Python tree will be retired.

## Build

```bash
make build            # → dist/whatskept (always re-signs after build)
```

The Makefile invokes `build/sign.sh` after `go build`. This is **not
optional** on Apple Silicon: post-link byte modifications by macOS tooling
can silently invalidate the linker-emitted ad-hoc signature, producing a
binary the kernel refuses to start (the process freezes with zero CPU time
and cannot be killed). Re-signing fixes it unconditionally.

First build is slow (~60 s) because `mattn/go-sqlite3` is a transitive cgo
dep that has to compile SQLite from C. Subsequent builds are sub-second.

## Run

```bash
./dist/whatskept --help
./dist/whatskept list
./dist/whatskept extract               # writes ./ChatStorage.sqlite
./dist/whatskept extract -o ~/Documents/my-whatsapp-chat
```

The `extract` subcommand reads the backup password from `$BACKUP_PASSWORD`
or a `.env` file in the output directory (or any parent), the same way the
Python implementation does. Never prompts.

## Layout

```
whatskept/
├── cmd/whatskept/      main binary, cobra subcommands
├── internal/
│   ├── backup/         iOS backup discovery + decryption
│   │                     wraps github.com/dunhamsteve/ios
│   └── secrets/        BACKUP_PASSWORD resolution (env + .env walk)
├── build/sign.sh       post-build codesign step (Apple Silicon)
├── Makefile
└── README.md (this file)
```

## Status

| Subcommand                | Python (wa-extract)       | Go (here)  |
| ------------------------- | ------------------------- | ---------- |
| `whatskept list`          | parts of `cli.py`         | done       |
| `whatskept extract`       | `cli.py` + `extract.py`   | basic      |
| `whatskept media-index`   | `media_indexer.py`        | pending    |
| `whatskept voice-index`   | `voice_indexer.py`        | pending    |
| `whatskept contacts-sync` | `contacts_sync.py`        | pending    |
| `whatskept profiles-sync` | `profiles_sync.py`        | pending    |
| `whatskept app`           | `app/main.py`             | pending    |
| views.sql + FTS rebuild   | `postprocess.py`          | pending    |
| AGENTS.md scaffold        | `postprocess.py`          | pending    |

## Validated foundations

Two feasibility spikes were run before the rewrite started; both passed
against a real iPhone 15 on iOS 26.3.1:

- `idevicebackup2` + libimobiledevice dylibs can be embedded in a Go
  binary, extracted to a cache on first run, and used to drive a fresh
  iOS backup over USB with no runtime Homebrew dependency.
- `github.com/dunhamsteve/ios` decrypts a 280 MB encrypted
  `ChatStorage.sqlite` end-to-end in ~5.5 s with row counts matching the
  Python pipeline exactly.

The spikes themselves were deleted after passing; the relevant code is
recoverable from the `wa-extract` repo's git history if needed.
