# WhatsKept

Agent-queryable WhatsApp history from an iOS backup, in Go.

A single self-contained binary. Drives iOS backups, decrypts WhatsApp's
ChatStorage.sqlite, and (eventually) feeds it into a searchable
SQLite + FTS5 workspace that an agent can query directly.

## Contents

- [What you can ask](#what-you-can-ask)
- [What this is (and what it isn't)](#what-this-is-and-what-it-isnt)
- [Download](#download)
- [How this was built](#how-this-was-built)
- [System requirements](#system-requirements)
- [Privacy](#privacy)
- [Build](#build)
- [Run](#run)

## What you can ask

Once the workspace is built, point an LLM coding agent at the folder
(Windsurf, Claude Code, Cursor, VS Code + Copilot, …) and ask. A few
examples of what becomes possible:

| Use case | Example prompt |
| --- | --- |
| **Find a photo or voice note you only vaguely remember** | *"Find the photo Sara sent of a handwritten recipe — I think it had cardamom in it."* |
| **Recover decisions from a busy group chat** | *"Pull every message in the House Reno group about the kitchen budget and tell me what we landed on."* |
| **Recall a specific fact someone sent you** | *"What dosage did Dr. Patel say for the antibiotic, and how many days?"* |
| **Track receipts, orders, and tracking numbers** | *"List every tracking number anyone sent me in the last 6 months and flag the ones I never confirmed."* |
| **Summarize a relationship or thread** | *"Summarize what my brother and I have talked about this year — what's been on his mind?"* |
| **Reconstruct a timeline** | *"Build a timeline of my 2023 — major events, trips, life changes — using only what's in WhatsApp."* |
| **Index recommendations friends have sent** | *"List every restaurant, book, and movie friends have recommended in the last 2 years, grouped by category."* |

## What this is (and what it isn't)

WhatsKept is a **data pipeline**, not an AI assistant. Its entire job
is to take an encrypted iOS backup and turn it into a clean, local,
agent-friendly workspace on disk.

**What it does**

- **Drives iOS backups** over USB via `idevicebackup2` so you can
  refresh the source without leaving the app.
- **Decrypts** the WhatsApp `ChatStorage.sqlite` and the media/voice
  blobs from the encrypted iOS backup, using your backup password.
- **Processes media locally**: OCR + image classification through
  Apple's Vision framework, voice-note transcription through
  `whisper.cpp` with Metal.
- **Normalizes** everything into a single SQLite database (with FTS5)
  alongside extracted `media/`, `voice/`, and `profiles/` folders,
  joined against your macOS Contacts so chats are readable.
- **Writes an `AGENTS.md`** and agent-ignore files so an LLM coding
  agent dropped into the workspace knows the schema and skips the
  heavy binary trees.

**What it does *not* do**

- **No built-in chat, no built-in LLM, no agent runtime.** WhatsKept
  never sends your messages to OpenAI, Anthropic, Google, or anyone
  else. It does not embed a model, does not call an inference API,
  does not "summarize your chats" on its own.
- **No cloud sync, no account, no telemetry.** The only outbound
  network request the binary ever makes is a one-time HTTPS download
  of the whisper model from HuggingFace, and only if you opt into
  voice transcription.
- **No querying for you.** Asking questions like *"what did Alice say
  about the trip?"* is the **agent's** job — you open the workspace
  in Windsurf / VS Code + Copilot / Claude Code / Cursor / etc. and
  let *that* tool read the SQLite database. WhatsKept's
  responsibility ends when the workspace is ready.
- **No modification of the source backup.** The encrypted iOS backup
  under `~/Library/Application Support/MobileSync/Backup/` is read
  only; WhatsKept never writes to it.

Think of it as the **plumbing between your iPhone and your AI agent**:
it turns a locked, encrypted iOS backup into a plain folder of
readable text, searchable messages, and transcribed voice notes —
then steps out of the way and lets the agent you already trust do
the thinking.

## Download

Pre-built **macOS arm64 (Apple Silicon)** binaries, ad-hoc signed.
First launch: right-click → **Open** to bypass Gatekeeper.

- **GUI app** — [`WhatsKept-darwin-arm64.app.zip`](https://github.com/alkait/WhatsKept/releases/latest/download/WhatsKept-darwin-arm64.app.zip)
  Unzip and drag `WhatsKept.app` into `/Applications`.
- **CLI binary** — [`whatskept-darwin-arm64.zip`](https://github.com/alkait/WhatsKept/releases/latest/download/whatskept-darwin-arm64.zip)
  For `whatskept extract` / `whatskept list` and scripted use.
- **All releases & changelogs** — [github.com/alkait/WhatsKept/releases](https://github.com/alkait/WhatsKept/releases)

One-liner CLI install into `/usr/local/bin`:

```bash
curl -L -o /tmp/whatskept.zip \
  https://github.com/alkait/WhatsKept/releases/latest/download/whatskept-darwin-arm64.zip \
  && unzip -o /tmp/whatskept.zip -d /tmp \
  && sudo install /tmp/whatskept /usr/local/bin/whatskept \
  && rm /tmp/whatskept.zip
```

> Prefer to build from source? See [Build](#build) below.

## How this was built

> Built in a weekend with **Claude Opus 4.7**, burning ~**$600** in
> tokens so you don't have to. Practically every line of code in this
> repo is AI-generated. I won't pretend I read it line by line — I
> didn't — but I stood behind **every architecture decision**: how
> the backup is decrypted, where secrets live, what crosses a
> network boundary, how the workspace is laid out, why the binary
> ships self-contained. The agent wrote the code; the design, the
> trade-offs, and the privacy posture are mine.

## System requirements

- **macOS 13.0 Ventura or later** on **Apple Silicon (arm64)** — the
  bundled Swift Vision helper is arm64-only, the embedded
  libimobiledevice dylibs come from `/opt/homebrew/*`, and the bundled
  `whisper-cli` is compiled with `GGML_METAL=ON`.
- **Full Disk Access** for WhatsKept.app (or your Terminal, if you
  launched from a shell) — required to read
  `~/Library/Application Support/MobileSync/Backup/`. Grant under
  System Settings → Privacy & Security → Full Disk Access.
- **An iOS backup of an iPhone/iPad with WhatsApp installed**, plus
  its **encryption password**. The extractor reads `$BACKUP_PASSWORD`
  or a `.env` file in the workspace; it never prompts.
- **USB connection to the device** if you want to drive a fresh
  backup from the Backups tab

## Privacy

WhatsKept is designed to keep your WhatsApp history on your machine.

**The good**

- **No telemetry, no analytics, no accounts.** The binary makes no
  network calls of its own. The GUI's HTTP server binds to
  `127.0.0.1` only — it is not reachable from other devices on your
  network.
- **All processing is on-device.** Image OCR + classification runs
  through Apple's Vision framework (`whatskept-vision`); voice
  transcription runs through `whisper.cpp` with Metal acceleration.
  Neither talks to a cloud service.
- **Backup password is never transmitted.** It's read from
  `$BACKUP_PASSWORD` or a `.env` file in the workspace, held in
  process memory for the lifetime of the app session, and cleared
  when you switch workspaces or quit. Not written anywhere by
  WhatsKept on its own.
- **One opt-in network call, ever.** The first time you run voice
  transcription, the ~574 MB whisper model is downloaded from
  HuggingFace over HTTPS and SHA-256 verified. After that, the app
  is fully offline.

**What to be cautious about**

- **The workspace contains *decrypted* WhatsApp data.** `ChatStorage.sqlite`,
  `media/`, `voice/`, and `profiles/` are plaintext on disk, and the
  Messages sync also joins your macOS Contacts (names + phone
  numbers) into the database so chats are readable. Anyone with
  read access to that folder (other macOS users, Time Machine
  backups, cloud-sync folders like iCloud Drive / Dropbox / Google
  Drive) can read every message, contact, photo, and voice note.
  Pick a workspace path accordingly — `~/Documents` is fine,
  `~/Dropbox` is not.
- **The agent reads the text, but not the raw files.** WhatsKept
  drops `.windsurfignore`, `.copilotignore`, and similar ignore
  files so agents stay out of the `media/`, `voice/`, and
  `profiles/` folders — the actual photos, audio files, and profile
  pictures are off-limits. What the agent *does* see is everything
  in the SQLite database: every message, every image's OCR'd text
  and classification labels, every voice-note transcript, and the
  contact names and numbers joined in. So when you ask a question,
  chunks of that chat history can be sent to the agent's LLM
  provider. **Trust the agent's privacy story before pointing it at
  the workspace.**
- **The `.env` file holds your backup password in plaintext.** It
  lives inside the workspace directory; don't commit it to git, and
  don't ship the workspace folder anywhere.
- **Workspace deletion is permanent.** The Delete button wipes the
  whole directory tree — there is no recycle bin, no undo. The
  encrypted iOS backup is untouched, so a fresh sync rebuilds, but
  any notes/state you kept in the workspace are gone.

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