# WhatsKept

Searchable, agent-queryable WhatsApp history from an iOS backup, in Go.

A single self-contained binary. Drives iOS backups, decrypts WhatsApp's
ChatStorage.sqlite, and (eventually) feeds it into a searchable
SQLite + FTS5 workspace that an agent can query directly.

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

## Run
```bash
make app
```