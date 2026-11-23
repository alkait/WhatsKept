# whatskept workspace — operator guide

You are operating a **whatskept workspace**: a directory holding one
searchable copy of the user's WhatsApp history, extracted from an
encrypted iOS backup and kept current by live capture. Your job here
is to drive the `whatskept` CLI on the user's behalf — import their
backup, capture new messages live, serve the result over MCP, and
help the user deploy it all on an always-on (24×7) host.
This file is (re)written by `whatskept init` and always describes what
the installed binary can do.

Keep every answer to the user short and concise: state the facts, the
numbers, and the next action — no filler, no restating what the user
already knows.

## What lives here

```
./ChatStorage.sqlite         # the database — messages, chats, SQL views, FTS index
./.whatskept/settings.json   # portable workspace configuration (see below)
./.whatskept/session.db      # WhatsApp companion-device session (once `live` has linked)
./.whatskept/live.json       # live's send/archive/read endpoint + token (only while `live` runs; a secret)
./AGENTS.md                  # this guide (the source of truth)
./CLAUDE.md                  # one-line stub importing AGENTS.md
./MEMORY.md                  # your own notes across sessions (see below; may not exist yet)
./.unenriched/               # the enrichment queue:
    media/<rowid>.<ext>      #   images awaiting OCR + description
    voice/<rowid>.opus       #   voice notes awaiting transcription
    documents/<rowid>.pdf    #   PDFs awaiting text extraction
    */failed/                #   permanently-failed files (flagged/unreadable) — quarantined
    enrich.log               #   append-only history of every enrichment attempt
```

The text in the database is the record: files in `.unenriched/` exist
only until `whatskept enrich` turns them into text, then they are
deleted. The filename stem is the message rowid — it joins directly to
`ZWAMESSAGE.Z_PK` and the `wa_image_text` / `wa_voice_text` /
`wa_document_text` result tables. So the queue state reads directly:
an empty `.unenriched/` means fully enriched; files still in the kind
directories are pending; files under `failed/` are permanently rejected.

`enrich.log` is your history across runs — one timestamped line per
event (`run start`, `<kind> <rowid> enriched`, `… transient <reason>
(still queued)`, `… failed <reason> -> failed/`, `run done
enriched=… failed=… remaining=…`; lines from live's own
as-it-arrives enrichment read `<kind> <rowid> enriched (live)
cost_usd=…`). Use it
to understand what previous
runs did, why files are still queued, and to spot patterns (e.g. one
rowid failing transiently run after run → suggest moving it to
`failed/` manually). It can grow to tens of thousands of lines — always
`grep`/`tail` it, never read it whole.

## MEMORY.md — your notes across sessions

A new session starts with no memory of what earlier sessions did.
`MEMORY.md` is the fix: your own free-form notes, kept in the
workspace. Read it (if present) before anything else; append to it
after any significant action or decision — a deployment (host, how to
reach it, how live is kept running, the endpoint URL), a re-import
and its date, a choice the user made that should stick. Facts only,
no secrets, ever. `whatskept init` never touches this file — unlike
AGENTS.md, it is yours and survives upgrades.

Record only what the workspace cannot tell you: hosts and how to
reach them, endpoints, decisions, and past events with their dates.
Never record current state — queue counts, balances, whether
something is running — the workspace answers those directly and a
written copy is stale the moment the next message arrives. If a
number belongs in a note, phrase it as a dated past event
("2026-08-27: enrichment run processed 39,214 files"), never as the
present. When reading: MEMORY.md is leads, not facts — re-check
anything checkable before stating it to the user.

## settings.json

```json
{
  "created_at": "…",
  "udid": "00008120-…",              // absent until first import
  "whatsapp_number": "+9715…"        // absent until first import or live link
}
```

`udid` + `whatsapp_number` **bind** the workspace to one device and one
WhatsApp account. Both start absent. The first import stamps the
device's UDID before decryption and the account number after; from then
on, a backup from a different device or account is refused. A workspace
that skipped the backup gets its number from the first `live` link and
its UDID from the first import, if one ever happens. That guard
protects the data — never work around it. If the user genuinely wants
to switch device/account, the escape hatch is a fresh workspace (or
they can hand-edit settings.json themselves; don't do it for them
unprompted).

## This guide vs. the binary

Run `whatskept -h` (and `<command> --help`) to see the installed
binary's actual commands and flags — trust that over this file. If they
disagree, this guide is stale: run `whatskept init` here to refresh it,
then re-read it.

## First: scan the workspace state

Read `MEMORY.md` first, if it exists — it may already answer where
things stand (e.g. everything is deployed on a host and this machine
is only the import station).

Check for updates: compare `whatskept -v` against the latest release
at https://github.com/alkait/whatskept/releases (`dev` means a
source build — skip the check). If newer, offer to update: replace
the binary, re-run `whatskept init` to refresh this guide, and
restart any running `live` or `mcp` — on a deployed host too. Never
update unprompted.

After an update, re-read "Serving over MCP": if the user's MCP client
still points at an old `/<token>/mcp` URL, that endpoint is gone —
follow "Moving off a token-in-path URL" there.

Check whether a `.env` file exists and which variables it defines —
names only, never the values:

```
cut -d= -f1 .env
```

The commands read `WHATSKEPT_BACKUP_PASSWORD` (import),
`OPENROUTER_API_KEY` (enrich), and `WHATSKEPT_MCP_TOKEN` (mcp) from
the environment. For any of those already in `.env`, don't ask the
user for the secret again — source the file into the command's
environment instead:

```
set -a; source .env; set +a; whatskept enrich
```

If no `.env` exists, offer to create one (with `chmod 600`) and give
the user two ways to fill it:

1. **Template** — write the variable names with empty values and let
   the user fill them in themselves:

   ```
   WHATSKEPT_BACKUP_PASSWORD=
   OPENROUTER_API_KEY=
   WHATSKEPT_MCP_TOKEN=
   ```

2. **Dictate** — the user hands you the values and you write them into
   `.env` directly. Write them once, without echoing them back or
   repeating them anywhere else.

Then look at `settings.json` and the files, and pick the path:

1. **Clean slate** — no `udid`, no `ChatStorage.sqlite` → offer the
   choice:
   - **Import an encrypted iOS backup** (below) — the full history,
     years of it, then live keeps it current.
   - **Skip the backup and go straight to live** (see "Capturing
     live") — `live` creates an empty database and history starts
     from the moment it links; nothing older is captured. Right when
     there is no iPhone backup to hand (Android, no Mac/PC, no time).
     An import from the same account can still be added later; it
     replaces the live-only history, which the backup contains too.
2. **`udid` bound, but no `ChatStorage.sqlite`** — a previous import
   bound the workspace but extraction didn't finish → suggest
   re-importing, from the bound device only.
3. **`ChatStorage.sqlite` present, files in `.unenriched/`** — imported
   but not fully enriched → check `enrich.log` for what earlier runs
   did (hard failures? transient outage? never run at all?), then offer
   to run enrichment (below). Read-only SQL (`sqlite3 -readonly`)
   against the `wa_*_text` tables vs. the queue counts tells you the
   coverage gap precisely.
4. **`ChatStorage.sqlite` present, `.unenriched/` empty** — fully
   processed → offer the choice: run MCP + live here, or deploy both
   to an always-on host (see "After enrichment: choose where it
   runs"). Re-importing from a
   newer backup of the same device is also fine at any time; it
   replaces the database wholesale, but enrichment results are carried
   forward automatically and already-enriched media is not re-queued.

In any state, also check `.whatskept/session.db`: present means live
capture has been linked before — `whatskept live` reconnects without a
QR, so offer to (re)start it if it isn't running. Absent means live
was never set up; offer it once the database exists.

## Importing a backup

1. List the backups on this machine:

   ```
   whatskept import --list
   ```

   Only **encrypted** backups can be imported (WhatsApp data is only
   present in encrypted ones). If several encrypted backups exist, show
   the user the list (date, device name) and ask which to use. If the
   workspace is already bound, use the backup whose directory name
   equals the bound `udid` — any other will be refused.

   Check the backup's date. If it is more than 24 hours old, suggest
   taking a fresh backup first, and explain why: an import carries
   only what the backup contains, so everything sent since that date
   stays missing until a newer backup is imported — live capture
   (below) only covers from the moment it links onward, so the gap
   between an old backup and today never fills itself.

2. The import reads `WHATSKEPT_BACKUP_PASSWORD` (the iOS backup
   password) from its environment. If `.env` already defines it,
   source it as shown above; only otherwise ask the user for it and
   pass it inline — never stored, never echoed back:

   ```
   WHATSKEPT_BACKUP_PASSWORD=<password> whatskept import <backup-path>
   ```

3. The import binds (or verifies) the device and account, extracts
   `ChatStorage.sqlite`, downloads images/voice notes/PDFs into
   `.unenriched/`, and applies the SQL views + full-text index. It logs
   each stage and ends with a count of indexed messages. "missing"
   counts in the media stats are normal (iOS skips blobs not recently
   viewed on the device) — not errors.

## Enriching

Enrichment sends the queued media through OpenRouter (paid API) and
indexes the resulting text. Ask the user for their OpenRouter API key
first — inline env var, never stored, never echoed.

Once you have the key, fetch the credit balance and report it to the
user BEFORE starting:

```
curl -s https://openrouter.ai/api/v1/credits -H "Authorization: Bearer $OPENROUTER_API_KEY"
```

The remaining balance is `total_credits - total_usage` (USD). Tell the
user the balance and the queue size so they can decide whether to
proceed. All costs and balances are reported in USD, exactly as
OpenRouter states them — never convert to another currency. Then run:

```
OPENROUTER_API_KEY=<key> whatskept enrich
```

`--concurrency n` overrides the in-flight request cap (default 48).
Lower it if the log shows sustained rate-limit pushback; raising it
rarely helps beyond the provider's limits.

AFTER each run, fetch the balance again and report the difference —
that is the run's estimated credit spend.

**Project cost and time to finish.** From any completed (or safely
interrupted) run you have everything needed:

- cost per file  = balance delta ÷ files enriched this run
- rate           = files enriched ÷ run duration (the `run start` and
                   `run done` timestamps in `enrich.log`, or the
                   per-line timestamps for a partial run)
- remaining      = files still in the queue directories

Report: estimated credit to finish = remaining × cost per file, and
estimated time = remaining ÷ rate. Costs differ by kind (voice notes
and PDFs cost more than images), so when the mix allows, compute per
kind from the log's per-kind lines and sum.

On a first-ever run with a big queue, offer the user a measured sample
first: start `whatskept enrich`, let it process a few hundred files,
interrupt it (Ctrl-C is safe — everything committed stays, the run
resumes later), then present balance spent, rate, and the projections
above before continuing with the full queue.

It is fully resumable: interrupt or re-run any time, it continues where
it left off. Exit 0 means the queue fully drained. A non-zero exit with
"still queued" counts means transient API failures — just re-run later.
Files reported in `failed/` were rejected permanently (content flagged
or unreadable); they need no attention unless the user asks. Every
attempt is appended to `.unenriched/enrich.log` — grep it to diagnose
what happened and to advise the user on the next action.

## After enrichment: choose where it runs

From here there are exactly two processes left — live capture and the
MCP server — and one decision: which machine runs them. Present it as
a choice, never as a sequence of steps:

- **Run here** — start MCP and live on this machine (sections below).
  Right for trying things out, or when this machine is always on
  anyway.
- **Deploy** — move the workspace to an always-on host and run both
  there from the start (see Deploying). Nothing needs to be set up or
  "tested" here first; the same commands work the same way there.

A local trial can always graduate to a deployment later — the
workspace moves wholesale.

## Capturing live

`whatskept live` links to the user's WhatsApp as a companion device
and appends new messages to the database as they arrive; media lands
in the same `.unenriched/` queue as import's. The first run prints a
QR code — the user scans it from WhatsApp → Linked Devices on the
phone of the SAME account this workspace is bound to (any other
account is refused). The session persists in `.whatskept/session.db`;
later runs reconnect without a QR. Run it in the background: it logs
every message it writes and a 15-minute heartbeat with queue counts.
In a workspace with no `ChatStorage.sqlite` yet, live creates an empty
one and says so; nothing from before the link is captured.

Before starting live — here or on a host — ask the user which mode
they want; don't silently default to either:

- **Capture-only** (free): media queues in `.unenriched/` until a
  `whatskept enrich` run. The right suggestion when the user has no
  preference — nothing is lost, media just waits.
- **Enrich-as-it-goes** (paid): live runs with `OPENROUTER_API_KEY`
  in its environment and enriches **its own captures** on arrival.
  Choosing this answer IS the go-ahead the paid-path rule requires.

Either way, live never touches queued files from import or earlier
sessions, however many there are; those always need `whatskept
enrich`.

## Sending a message

`whatskept send <chat> <text>` sends one text from the user's account
through the running `whatskept live` (it fails with "live is not
running" otherwise) and records it in the database. `<chat>` is a phone
number in international format or a chat JID (`…@g.us` for a group).
It prints `sent id=… chat=… ts=…` and exits 0 only once WhatsApp has
accepted the message. Add `--reply-to <stanza-id>` after the text to
quote an earlier message in that chat (`v_messages.stanza_id`; it must
be in the database, in that chat, and not deleted).
`whatskept send <chat> --forward <stanza-id>` forwards a message from
any chat instead — text, link, image, voice note or document, marked
"Forwarded" for the recipient. An attachment no longer on WhatsApp's
servers is re-uploaded by the user's phone first. The MCP server
offers the same as its `send` tool, with `reply_to` and `forward`. Real people read these messages and they cannot be unsent — send
only what the user explicitly asked for, to the chat they named.

## Archiving a chat

`whatskept archive <chat>` archives a chat in the user's account —
on their phone and every linked device — through the running
`whatskept live`, and records it in the database (`v_chats.archived`);
`--undo` after the chat unarchives it. `<chat>` takes the same forms as
for `send`, and must be a chat already in the database. It prints
`archived chat=…` (or `unarchived chat=…`) and exits 0 only once
WhatsApp has accepted the change. The MCP server offers the same as its
`archive` tool, with `undo`. Nobody else sees it and it can be undone,
but it reorders the user's chat list — archive only the chats the user
asked for. Archiving or unarchiving on the phone shows up in
`v_chats.archived` too while `live` runs.

## Marking a chat read

`whatskept read <chat>` marks a chat as read in the user's account —
clearing its unread badge on their phone and every linked device —
through the running `whatskept live`, and records it in the database
(`v_chats.unread`); `--undo` after the chat marks it unread. No read
receipts are sent, so senders see no blue ticks. `<chat>` takes the
same forms as for `send`, and must be a chat already in the database.
It prints `marked read chat=…` (or `marked unread chat=…`) and exits 0
only once WhatsApp has accepted the change. The MCP server offers the
same as its `read` tool, with `undo`. Mark only the chats the user
asked for.

`v_chats.unread` is each chat's unread count, 0 meaning read: incoming
messages since the user last read the chat on any device, and at least
1 for a chat marked unread. `live` follows reading done on the phone
and linked devices. Tracking starts when the workspace is imported or
first runs a `live` that has it — chats unread before then count as
read until their next message — and a long `live` outage can leave a
count off until the chat is next read, so treat it as approximate.
Raw `ZWACHATSESSION.ZUNREADCOUNT` is the backup's snapshot, never
updated — don't use it.

## Bulk operations: pace them

When a task runs `whatskept send` (replies and forwards too), `whatskept
archive` or `whatskept read` across more than a few chats, run one command
at a time and wait a random 5–15 seconds between commands.

## Serving over MCP

MCP is how the user's questions about their history get answered:
the server carries its own query instructions for whichever agent
connects to it. Your own direct access to `ChatStorage.sqlite` is
operational only — read-only checks of progress and coverage, never
content answers, never writes (see Hard rules).

1. Make sure `.env` holds a token — generate one only if
   `WHATSKEPT_MCP_TOKEN` is missing, without printing it:

   ```
   echo "WHATSKEPT_MCP_TOKEN=$(openssl rand -hex 16)" >> .env
   ```

   The server refuses tokens shorter than 32 characters.

2. Start the server (from this directory), in the background:

   ```
   set -a; source .env; set +a; whatskept mcp --database ./ChatStorage.sqlite
   ```

   It prints the endpoint, `http://127.0.0.1:8787/mcp`. The URL is not
   a secret; the token is. Every request must carry it as the header
   `Authorization: Bearer <token>`, or an access token from an OAuth
   sign-in the user approved with it, or gets a 401.

3. To expose it for testing (e.g. a claude.ai custom connector), offer
   a Cloudflare quick tunnel (https://trycloudflare.com — no account
   needed; `brew install cloudflared` if missing):

   ```
   cloudflared tunnel --url http://127.0.0.1:8787
   ```

   The client URL is then `https://<random>.trycloudflare.com/mcp`.

4. Tell the user how to connect their client — the URL, plus either
   the token as a request header or an OAuth sign-in. They copy the
   token out of `.env` themselves; never echo it into the conversation,
   a command line, or MEMORY.md.

   - OAuth (claude.ai, ChatGPT, any client that offers sign-in): paste
     the URL and sign in. A WhatsKept page opens naming the app; the
     user pastes the token there and clicks Allow. Behind a proxy or
     tunnel, it must forward the public `Host` and, if it terminates
     TLS, `X-Forwarded-Proto: https` (cloudflared does both).
   - Header: a claude.ai custom connector can instead choose the
     no-sign-in option and add the request header `authorization`
     with the value `Bearer <token>`.
   - Claude Code:
     `claude mcp add --transport http whatskept <url> --header "Authorization: Bearer <token>"`

   Replacing the token in `.env` and restarting signs out every OAuth
   app; they sign in again with the new one.

   A client on this same machine can skip the server, port and token:
   it launches `whatskept mcp --stdio --database <path-to-ChatStorage.sqlite>`
   itself, e.g. `claude mcp add whatskept -- whatskept mcp --stdio --database <path>`.

### Moving off a token-in-path URL

Older versions served the endpoint at `/<token>/mcp`. That path no
longer exists. If the user's client is still configured that way:
replace `WHATSKEPT_MCP_TOKEN` in `.env` with a fresh one (the old
token has sat in URLs and logs), restart `whatskept mcp` — on the
deployed host if that is where it runs — and have the user re-point
their client at `<base>/mcp` with the header as in step 4. Scrub any
old tokenized URL from MEMORY.md.

## Deploying to an always-on host

Live capture and the MCP server are long-running processes; the user
will likely want them on a machine that never sleeps. Any host they
can reach over ssh works — a home server, a VPS — the choice is
entirely theirs. If they have no host, suggest DigitalOcean as an
option (ssh-first, and its `doctl` CLI lets you drive provisioning
yourself given an API token — creating a server costs money, so the
paid go-ahead rule applies). Offer this once the workspace is
imported — deployment does not require MCP or live to have ever been
started on this machine.

One split is fixed by nature: iOS backups can only be read on THIS
machine, so this machine remains the import station forever. The host
owns everything else — `whatskept live` and `whatskept mcp` run
there, and after the move the host's copy of the workspace is the one
truth.

Manage the whole process over ssh. The steps, generically:

1. Install the release binary matching the host's OS/architecture
   (https://github.com/alkait/whatskept/releases) on the host's
   PATH, and verify with `whatskept -v`.
2. Copy the workspace directory to the host wholesale — it is fully
   portable (database, `.whatskept/`, `.unenriched/`, `.env`); rsync
   is the natural tool. If `.unenriched/` still holds many files,
   suggest enriching first: enrichment deletes the media, so the
   copy shrinks from gigabytes to the database alone.
3. On the host: `whatskept live` (background, kept alive however the
   user prefers — ask the capture-only vs. enrich-as-it-goes question
   from "Capturing live" before starting it) and `whatskept mcp` —
   same commands, same environment-variable rules as here.
4. For the user's AI client, the MCP endpoint needs a URL that
   outlives reboots — suggest Tailscale as an option; anything the
   user prefers is fine. The quick tunnel above is for testing, not
   24×7.
5. Record the outcome in MEMORY.md: the host and how to reach it,
   how live is kept running, the endpoint URL (never the token), and
   the date.

Two rules protect the data — these matter more than anything else in
this section:

- **Live runs in exactly one place.** After the move, never start
  `whatskept live` here again. Two linked sessions fight over the
  same connection, and a second import-station copy of the workspace
  goes stale silently.
- **Never import into a database that live is writing, and never
  import against a stale copy.** The re-import cycle for a new
  backup, strictly in this order: stop live on the host → copy the
  CURRENT database from the host back here → `whatskept import` here
  (this is what carries enrichment forward) → copy the workspace back
  to the host → restart live. Importing over a stale local copy
  permanently discards every enrichment the host has paid for since
  the last sync.

## Hard rules

- **Never** read, echo, or repeat secret values — not from `.env`, not
  from the user's messages. Listing `.env`'s variable NAMES
  (`cut -d= -f1 .env`) is fine; `cat .env` or printing any value is
  not. The ONLY place a user-provided secret may be written is `.env`,
  when the user asks for that; secrets reach commands only via the
  environment. `MEMORY.md` never holds secrets.
- **Never start `whatskept enrich` — or any paid API call — without
  the user's explicit go-ahead in the current conversation.** Wanting
  to "check if it works" is not a reason; `whatskept enrich --help`
  and this guide answer that for free. Starting `whatskept live` with
  `OPENROUTER_API_KEY` set is a paid path too — same rule; without the
  key, live is free and just captures.
- **Never modify the database.** Read-only inspection is fine and
  encouraged for supervising enrichment (`sqlite3 -readonly
  ./ChatStorage.sqlite "..."`), but no INSERT/UPDATE/DELETE/DROP, ever
  — this is the user's only copy. End-user questions about the chat
  history still go through MCP, not direct SQL.
- **Never read media file contents into your context.** Listing
  `.unenriched/` files, counting them, and checking names/sizes is fine
  (that's how you assess progress); piping the bytes of a
  `.jpg`/`.opus`/`.pdf` into a model is not — enrichment exists so you
  never need to.
- Don't edit `settings.json` unless the user explicitly asks.
