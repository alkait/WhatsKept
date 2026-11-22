# whatskept

One lean Go repo, one binary, consolidating whatskept, whatskept-mcp and
whatskept-live. No GUI. No backup/restore features. No deployment concerns.

## Shape

Everything lives in a workspace — a directory marked by a `.whatskept/`
directory, created with `whatskept init [dir]`. Every command works
inside one, except `mcp`, which addresses the database file explicitly.
`.whatskept/` contains `settings.json` — portable configuration,
including the device/account binding — and, once `live` has linked,
`session.db` (the companion-device session), plus `live.json` while
`live` runs. All other state (the shared
SQLite database, etc.) sits at the workspace root. Secrets come from
`.env` or the environment, never from settings:

- `whatskept import <ios-backup-path>` — seed history from an iOS backup,
  enrich everything, resumable. First import binds the workspace to that
  device and WhatsApp account; a different device's or account's backup is
  refused thereafter.
- `whatskept live` — capture new messages as they arrive, enriched through
  the same pipeline.
- `whatskept send <chat> <text> [--reply-to <stanza-id>]` — send a text
  through the running `live`, which owns the connection and records the
  message; `--reply-to` quotes an earlier message in that chat.
  `whatskept send <chat> --forward <stanza-id>` forwards a message from
  any chat instead — attachments by their stored download details, the
  phone re-uploading any the CDN has dropped. The hand-off is a
  loopback endpoint advertised in `.whatskept/live.json` while live runs.
- `whatskept archive <chat> [--undo]` — archive (or unarchive) a chat
  through the running `live`, the same hand-off; `live` also records
  archiving done on the phone.
- `whatskept read <chat> [--undo]` — mark a chat read (or unread) on the
  user's own devices through the running `live`, the same hand-off; no
  read receipts are sent.
- `whatskept mcp --database <file>` — the only query surface; serves the
  unified DB over HTTP at `/mcp`, behind a bearer token (or over stdio with
  `--stdio`, for clients that launch it). Read-only, plus `send`,
  `archive` and `read` tools that hand off to the live running beside the database.

Enrichment (image descriptions/OCR, voice transcripts, PDF text) runs via
OpenRouter on both import and live paths. Media, PDFs and voice files are
deleted once enriched — the text in the DB is the record.

## Philosophy

- **Less is more.** Minimal code, minimal commands, minimal README.
- **Small increments.** Build one small, agreed slice at a time; after every
  change, run `go test ./...` and `go build` so the tests and the binary at
  the repo root always reflect the current code.
- **No implementation without explicit approval.** A question, a design
  discussion, or an invitation to suggest options is never a green light to
  write code. Answer, propose, and wait for an explicit go-ahead.
- **Agent-first.** The CLI is operated by AI agents: non-interactive, clear
  parseable logs, honest exit codes, resumable. No interactive prompts.
- **Always on main.** All work happens directly on `main` — no branches,
  no PRs. Releases are cut manually from the Actions tab. Never commit
  or push without the user asking for it.
- **One DB, one truth.** Import seeds it, live appends to it, mcp reads it.
- **Tested for real.** `whatsapp-tester` (in-repo dev tool at
  `cmd/whatsapp-tester`, paired to a second WhatsApp number, never released)
  drives end-to-end tests against the real account — no asking a human to
  send messages.

## Testing pattern

Every feature is covered the same way, all under one `go test ./...`:

- **Function tests** — call the Go functions directly against temp dirs.
- **Binary tests** — build the real binary once (in `TestMain`), run it as
  a subprocess, assert exit codes, output wording, and disk state.
- **Fake the external world** — features that depend on it get fakes:
  fixture backups in `testdata/`, a local `httptest` stand-in for the
  enrichment API. No test needs a phone, a key, or the network.
- **Real e2e is gated** — the suite in `e2e-test/` sits behind the `e2e`
  build tag and runs only on explicit request:
  `go test -tags e2e ./e2e-test -workspace <dir>`. It needs the tester's
  paired session (`e2e-test/session.db`, gitignored) and `whatskept live`
  running in that workspace; the destination number is derived from the
  workspace binding — e2e traffic is strictly between the tester number
  and the workspace account, in chats the suite creates. Plain
  `go test ./...` never touches it and stays green on any machine.
