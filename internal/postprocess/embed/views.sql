-- views.sql
-- Convenience views + FTS5 index applied to a freshly extracted ChatStorage.sqlite.
-- This file is idempotent: re-applying it drops and recreates everything it owns.
-- Read-only against the underlying Z* tables; never modifies them.

PRAGMA foreign_keys = OFF;

-- ----------------------------------------------------------------------------
-- wa_contact: optional iOS-Contacts mapping (populated by `whatskept-contacts-sync`).
--   - jid:           '<digits>@s.whatsapp.net'
--   - display_name:  the user's chosen label from their iOS Contacts app.
--   - source:        provenance, currently always 'ios-contacts'.
-- We CREATE IF NOT EXISTS so v_messages can LEFT JOIN it unconditionally,
-- whether or not the contacts-sync step has run yet.
-- ----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS wa_contact (
    jid           TEXT PRIMARY KEY,
    display_name  TEXT NOT NULL,
    source        TEXT NOT NULL,
    synced_at     TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS wa_contact_display_name_idx ON wa_contact(display_name);


-- ----------------------------------------------------------------------------
-- wa_jid_alias: LID ↔ phone-JID bridge.
--
-- Newer WhatsApp builds increasingly identify users by an opaque LID
-- (`<digits>@lid`) instead of their phone JID (`<digits>@s.whatsapp.net`).
-- Group messages, in particular, often arrive with a LID sender, while the
-- user's saved iOS-Contacts entries are keyed by phone JID. Without a
-- bridge, "الوالد" and "F الوالدة" (saved in iOS Contacts under their
-- phone numbers) get masked by the partner's self-chosen WhatsApp push
-- name in any group context.
--
-- The bridge is `ZWACHATSESSION`: every 1:1 chat session that exists with
-- a partner has both ZCONTACTJID (phone) and ZCONTACTIDENTIFIER (LID),
-- so we extract that pairing into a tiny lookup view. ~2,200 mappings
-- in a typical heavy user's DB.
-- ----------------------------------------------------------------------------
DROP VIEW IF EXISTS wa_jid_alias;
CREATE VIEW wa_jid_alias AS
SELECT
    ZCONTACTJID         AS phone_jid,
    ZCONTACTIDENTIFIER  AS lid_jid
FROM   ZWACHATSESSION
WHERE  ZCONTACTJID         LIKE '%@s.whatsapp.net'
  AND  ZCONTACTIDENTIFIER  LIKE '%@lid';


-- ----------------------------------------------------------------------------
-- wa_document: one row per WhatsApp "document" message (ZMESSAGETYPE = 8).
--   - filename:  the original filename the sender chose (e.g. "contract_v3.pdf"),
--                stored cleartext in ZWAMEDIAITEM.ZAUTHORNAME for ~98% of docs.
--   - ext:       file extension parsed off ZMEDIALOCALPATH ('pdf','docx',...).
--   - file_size: bytes, from ZWAMEDIAITEM.ZFILESIZE.
--
-- This is purely derived metadata — we do NOT extract document content. The
-- filename alone makes "did Khalid send me that passport scan?" answerable
-- via messages_fts, which is the bulk of casual document recall.
--
-- Repopulated every time views.sql is applied; no separate indexer/CLI needed.
-- ----------------------------------------------------------------------------
DROP TABLE IF EXISTS wa_document;
CREATE TABLE wa_document (
    rowid     INTEGER PRIMARY KEY,
    filename  TEXT,
    ext       TEXT,
    file_size INTEGER
);

INSERT INTO wa_document(rowid, filename, ext, file_size)
SELECT
    mi.ZMESSAGE,
    NULLIF(mi.ZAUTHORNAME, ''),
    -- Extract the extension as the substring after the LAST '.' in the path.
    -- SQLite has no REVERSE/string-rfind: this idiom works by RTRIM-ing the
    -- path against (the path with all dots removed) — RTRIM stops at the
    -- first dot from the right, leaving everything up to and including that
    -- dot. We then SUBSTR off the tail. Falls back to NULL when the path
    -- has no dot at all.
    LOWER(CASE
        WHEN mi.ZMEDIALOCALPATH IS NULL OR INSTR(mi.ZMEDIALOCALPATH, '.') = 0 THEN NULL
        ELSE SUBSTR(
            mi.ZMEDIALOCALPATH,
            LENGTH(RTRIM(mi.ZMEDIALOCALPATH, REPLACE(mi.ZMEDIALOCALPATH, '.', ''))) + 1
        )
    END),
    mi.ZFILESIZE
FROM   ZWAMESSAGE   m
JOIN   ZWAMEDIAITEM mi ON mi.ZMESSAGE = m.Z_PK
WHERE  m.ZMESSAGETYPE = 8;

CREATE INDEX IF NOT EXISTS wa_document_ext_idx      ON wa_document(ext);
CREATE INDEX IF NOT EXISTS wa_document_filename_idx ON wa_document(filename);


-- ----------------------------------------------------------------------------
-- wa_profile_picture: per-JID avatar paths, populated by
-- `whatskept-profiles-sync`.
--
--   - whatsapp_path / whatsapp_kind / whatsapp_picture_id
--       The contact's self-chosen WhatsApp profile picture, decrypted out
--       of the iOS backup's `Media/Profile/` directory. `kind` is 'jpg'
--       (full-resolution) or 'thumb' (cached preview); we prefer 'jpg'
--       when available and fall back to 'thumb'.
--
-- Schema is also `CREATE TABLE IF NOT EXISTS`'d inside `profiles.py`, so
-- `whatskept-profiles-sync` works on a workspace where views.sql has
-- never been applied. We use `CREATE TABLE IF NOT EXISTS` here too (not
-- `DROP TABLE IF EXISTS`) so re-applying views.sql doesn't wipe a sync
-- the user already ran.
-- ----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS wa_profile_picture (
    jid                  TEXT PRIMARY KEY,
    whatsapp_path        TEXT,
    whatsapp_kind        TEXT,
    whatsapp_picture_id  TEXT,
    synced_at            TEXT NOT NULL
);


-- ----------------------------------------------------------------------------
-- wa_ios_avatar: iOS Contacts thumbnails for JIDs the user has saved in
-- their address book. Populated by `whatskept-contacts-sync`, which
-- snapshot-rebuilds the table on every run.
--
--   path:  workspace-relative path (e.g. 'profiles/ios/<jid>.jpg').
--   kind:  always 'thumb' for now (we extract ABThumbnailImage only).
--
-- Like `wa_profile_picture`, declared with CREATE IF NOT EXISTS so this
-- file is safe to re-apply without wiping a sync the user already ran.
-- The companion `contacts.ensure_schema` mirrors the same DDL so the sync
-- still works on a workspace where views.sql hasn't been refreshed.
-- ----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS wa_ios_avatar (
    jid        TEXT PRIMARY KEY,
    path       TEXT NOT NULL,
    kind       TEXT NOT NULL,
    synced_at  TEXT NOT NULL
);


-- ----------------------------------------------------------------------------
-- v_avatar: one row per JID that has *some* avatar — WhatsApp first, iOS
-- Contacts as fallback.
--
--   path:    workspace-relative path. Always a `.jpg`.
--   source:  'whatsapp' or 'ios' so the caller can tell which won.
--
-- Resolution: per (canonical) JID, pick the WhatsApp avatar if any, else
-- the iOS Contacts thumbnail. WhatsApp avatars are also exposed under
-- their LID↔phone alias forms so a sender_jid in either flavour resolves.
-- iOS avatars are always phone-keyed (since iOS Contacts is keyed by
-- phone number), and are likewise exposed under the LID alias so group
-- senders that arrive as `<digits>@lid` still find their saved avatar.
--
-- Stable agent contract: `SELECT path FROM v_avatar WHERE jid = ?`
-- returns 0 or 1 row.
-- ----------------------------------------------------------------------------
DROP VIEW IF EXISTS v_avatar;
CREATE VIEW v_avatar AS
WITH unified(jid, path, source, synced_at) AS (
    -- WhatsApp avatars: direct.
    SELECT p.jid, p.whatsapp_path, 'whatsapp', p.synced_at
    FROM   wa_profile_picture p
    WHERE  p.whatsapp_path IS NOT NULL

    UNION ALL

    -- WhatsApp avatars: phone JID exposed under LID alias.
    SELECT a.lid_jid, p.whatsapp_path, 'whatsapp', p.synced_at
    FROM   wa_profile_picture p
    JOIN   wa_jid_alias       a ON a.phone_jid = p.jid
    WHERE  p.whatsapp_path IS NOT NULL

    UNION ALL

    -- WhatsApp avatars: LID exposed under phone alias.
    SELECT a.phone_jid, p.whatsapp_path, 'whatsapp', p.synced_at
    FROM   wa_profile_picture p
    JOIN   wa_jid_alias       a ON a.lid_jid = p.jid
    WHERE  p.whatsapp_path IS NOT NULL

    UNION ALL

    -- iOS Contacts avatars: direct (always phone-keyed).
    SELECT i.jid, i.path, 'ios', i.synced_at
    FROM   wa_ios_avatar i

    UNION ALL

    -- iOS Contacts avatars: phone JID exposed under LID alias, so a
    -- group sender that arrives as `<digits>@lid` still finds the saved
    -- avatar even when WhatsApp has no profile pic for that contact.
    SELECT a.lid_jid, i.path, 'ios', i.synced_at
    FROM   wa_ios_avatar  i
    JOIN   wa_jid_alias   a ON a.phone_jid = i.jid
)
SELECT
    jid,
    -- WhatsApp wins per JID. COALESCE selects the WhatsApp path if any
    -- row of source='whatsapp' exists for this JID; otherwise the iOS one.
    COALESCE(
        MAX(CASE WHEN source = 'whatsapp' THEN path END),
        MAX(CASE WHEN source = 'ios'      THEN path END)
    )                                                      AS path,
    CASE
        WHEN MAX(CASE WHEN source = 'whatsapp' THEN 1 END) = 1
            THEN 'whatsapp'
        ELSE 'ios'
    END                                                    AS source,
    MAX(synced_at)                                         AS synced_at
FROM   unified
GROUP  BY jid
HAVING COALESCE(
    MAX(CASE WHEN source = 'whatsapp' THEN path END),
    MAX(CASE WHEN source = 'ios'      THEN path END)
) IS NOT NULL;


-- ----------------------------------------------------------------------------
-- v_chats: one row per chat session (1:1 or group).
--   - Cocoa epoch (seconds since 2001-01-01 UTC) is converted to Unix epoch.
--   - kind: 'group' if the JID ends in @g.us, otherwise 'dm'.
-- ----------------------------------------------------------------------------
DROP VIEW IF EXISTS v_chats;
CREATE VIEW v_chats AS
SELECT
    cs.Z_PK                                                    AS chat_id,
    cs.ZCONTACTJID                                             AS jid,
    -- For DM chats, resolve the partner's display name in this strict
    -- preference order (the USER's saved name wins over the partner's
    -- self-chosen name; opaque identifiers are last resort):
    --   1. iOS Contacts label (wa_contact, populated by
    --      `whatskept-contacts-sync`) — what *you* called this person in
    --      iOS Contacts. The most authoritative "saved" name.
    --   2. ZPARTNERNAME, but ONLY when it's a real name. WhatsApp mirrors
    --      iOS Contacts here, but for unsaved contacts it falls back to
    --      the formatted phone string ("+971 50 129 1247"). We treat
    --      anything matching '+<digits/spaces>' as a phone fallback and
    --      skip it; anything else is treated as another saved-contact
    --      mirror. This recovers ~324 contacts that are saved on the
    --      device but missing from `wa_contact`.
    --   3. WhatsApp push name keyed by the phone JID — the partner's
    --      self-chosen WhatsApp display name.
    --   4. WhatsApp push name keyed by the partner's LID alias
    --      (cs.ZCONTACTIDENTIFIER — newer WhatsApp builds store the
    --      profile push under a '<digits>@lid' identifier instead of the
    --      phone JID). Same surface (push name), different lookup key.
    --   5. Raw JID as last-resort fallback (never a phone-string
    --      ZPARTNERNAME — the JID is more useful: stable, joinable).
    -- For groups, just use the group title; wa_contact / ZWAPROFILEPUSHNAME
    -- only hold individual JIDs.
    CASE
        WHEN cs.ZCONTACTJID LIKE '%@g.us' THEN cs.ZPARTNERNAME
        ELSE COALESCE(
            (SELECT display_name FROM wa_contact      WHERE jid = cs.ZCONTACTJID),
            CASE
                WHEN cs.ZPARTNERNAME IS NULL OR cs.ZPARTNERNAME = '' THEN NULL
                WHEN cs.ZPARTNERNAME GLOB '+[0-9 ]*'                 THEN NULL
                ELSE cs.ZPARTNERNAME
            END,
            (SELECT ZPUSHNAME FROM ZWAPROFILEPUSHNAME WHERE ZJID = cs.ZCONTACTJID),
            (SELECT ZPUSHNAME FROM ZWAPROFILEPUSHNAME WHERE ZJID = cs.ZCONTACTIDENTIFIER),
            cs.ZCONTACTJID
        )
    END                                                        AS title,
    CASE WHEN cs.ZCONTACTJID LIKE '%@g.us' THEN 'group' ELSE 'dm' END AS kind,
    cs.ZMESSAGECOUNTER                                         AS message_count,
    datetime(cs.ZLASTMESSAGEDATE + 978307200, 'unixepoch')     AS last_message_at,
    cs.ZARCHIVED                                               AS archived
FROM ZWACHATSESSION cs;


-- ----------------------------------------------------------------------------
-- v_messages: flattened message view.
--   - ts: human-readable UTC timestamp (Cocoa epoch already converted).
--   - sender_name: best-effort. For outgoing messages, 'me'. For group
--     messages, the group-member contact/push name. For 1:1 incoming,
--     the chat partner name or push name.
--   - is_from_me: 0 = received, 1 = sent.
--   - reply_to_id: FK back to v_messages.rowid for quoted-reply chains.
--   - text: ZWAMESSAGE.ZTEXT verbatim. NULL for most media messages.
--   - message_type_name: human label for common ZMESSAGETYPE values.
--   - link_url / link_title: populated for 'link' messages (type 7).
--     link_title is the preview headline WhatsApp fetched at send time.
-- ----------------------------------------------------------------------------
DROP VIEW IF EXISTS v_messages;
CREATE VIEW v_messages AS
SELECT
    m.Z_PK                                                     AS rowid,
    m.ZCHATSESSION                                             AS chat_id,
    -- chat_title mirrors v_chats.title — see that view's comment for the
    -- full rationale. Order: iOS-Contacts (wa_contact) > non-phone
    -- ZPARTNERNAME > push-name (phone JID) > push-name (LID alias) > raw JID.
    CASE
        WHEN cs.ZCONTACTJID LIKE '%@g.us' THEN cs.ZPARTNERNAME
        ELSE COALESCE(
            (SELECT display_name FROM wa_contact      WHERE jid = cs.ZCONTACTJID),
            CASE
                WHEN cs.ZPARTNERNAME IS NULL OR cs.ZPARTNERNAME = '' THEN NULL
                WHEN cs.ZPARTNERNAME GLOB '+[0-9 ]*'                 THEN NULL
                ELSE cs.ZPARTNERNAME
            END,
            (SELECT ZPUSHNAME FROM ZWAPROFILEPUSHNAME WHERE ZJID = cs.ZCONTACTJID),
            (SELECT ZPUSHNAME FROM ZWAPROFILEPUSHNAME WHERE ZJID = cs.ZCONTACTIDENTIFIER),
            cs.ZCONTACTJID
        )
    END                                                        AS chat_title,
    CASE WHEN cs.ZCONTACTJID LIKE '%@g.us' THEN 'group' ELSE 'dm' END AS chat_kind,
    datetime(m.ZMESSAGEDATE + 978307200, 'unixepoch')          AS ts,
    m.ZMESSAGEDATE                                             AS ts_cocoa,
    m.ZISFROMME                                                AS is_from_me,
    -- sender_name resolution. Tiered so the USER's saved name always
    -- wins over the sender's self-chosen WhatsApp name:
    --
    --   Tier 1 — saved (the user's address book):
    --     a. wa_contact.display_name — the live iOS-Contacts label
    --        (populated by `whatskept-contacts-sync`).
    --     b. (groups only) ZWAGROUPMEMBER.ZCONTACTNAME — WhatsApp's own
    --        mirror of iOS Contacts at the per-member level.
    --     c. (groups only) ZWAGROUPMEMBER.ZFIRSTNAME — first-name only,
    --        from the same WhatsApp contact-sync mirror.
    --     d. (DMs only) ZWACHATSESSION.ZPARTNERNAME — only when it's a
    --        real name. We skip it when it's a bare phone string
    --        ("+971 50 ..."), which is what WhatsApp falls back to for
    --        contacts the user hasn't saved.
    --
    --   Tier 2 — push name (the sender's self-chosen WhatsApp display
    --   name). Stored in ZWAPROFILEPUSHNAME, keyed by either the phone
    --   JID or, for newer WhatsApp builds, the partner's LID alias
    --   (ZCONTACTIDENTIFIER, format '<digits>@lid'). We try both.
    --
    --   Tier 3 — raw JID as last-resort fallback.
    --
    -- ⚠️  We do NOT use ZWAMESSAGE.ZPUSHNAME — in current iOS WhatsApp
    --     builds that column holds opaque protobuf bytes, not the push
    --     name. The real push name lives in ZWAPROFILEPUSHNAME.
    --
    -- 'me' short-circuits everything for outgoing messages.
    CASE
        WHEN m.ZISFROMME = 1 THEN 'me'
        WHEN gm.ZMEMBERJID IS NOT NULL THEN COALESCE(
            -- Tier 1a: iOS-Contacts saved name, direct lookup.
            (SELECT display_name FROM wa_contact         WHERE jid  = gm.ZMEMBERJID),
            -- Tier 1b: iOS-Contacts via LID→phone bridge. If gm.ZMEMBERJID
            -- is a LID, find the user's saved name keyed under the
            -- corresponding phone JID. This is what lets "الوالد" /
            -- "F الوالدة" surface in groups instead of the partner's
            -- self-chosen push name like "Mohd Alkait".
            (SELECT wc.display_name
               FROM wa_contact wc
               JOIN wa_jid_alias a ON a.phone_jid = wc.jid
              WHERE a.lid_jid = gm.ZMEMBERJID),
            -- Tier 1c: WhatsApp's per-member iOS-Contacts mirror.
            NULLIF(gm.ZCONTACTNAME, ''),
            NULLIF(gm.ZFIRSTNAME,   ''),
            -- Tier 2: push name (direct).
            (SELECT ZPUSHNAME    FROM ZWAPROFILEPUSHNAME WHERE ZJID = gm.ZMEMBERJID),
            -- Tier 2b: push name via LID→phone bridge.
            (SELECT p.ZPUSHNAME
               FROM ZWAPROFILEPUSHNAME p
               JOIN wa_jid_alias a ON a.phone_jid = p.ZJID
              WHERE a.lid_jid = gm.ZMEMBERJID),
            -- Tier 3: JID.
            gm.ZMEMBERJID
        )
        ELSE COALESCE(
            -- Tier 1a: iOS-Contacts saved name, direct lookup.
            (SELECT display_name FROM wa_contact         WHERE jid  = m.ZFROMJID),
            -- Tier 1b: iOS-Contacts via LID→phone bridge (in case a DM
            -- message arrives with a LID sender).
            (SELECT wc.display_name
               FROM wa_contact wc
               JOIN wa_jid_alias a ON a.phone_jid = wc.jid
              WHERE a.lid_jid = m.ZFROMJID),
            -- Tier 1c: ZPARTNERNAME (skip phone-string fallbacks).
            CASE
                WHEN cs.ZPARTNERNAME IS NULL OR cs.ZPARTNERNAME = '' THEN NULL
                WHEN cs.ZPARTNERNAME GLOB '+[0-9 ]*'                 THEN NULL
                ELSE cs.ZPARTNERNAME
            END,
            -- Tier 2: push name, direct on ZFROMJID and via the chat
            -- session's LID alias.
            (SELECT ZPUSHNAME    FROM ZWAPROFILEPUSHNAME WHERE ZJID = m.ZFROMJID),
            (SELECT ZPUSHNAME    FROM ZWAPROFILEPUSHNAME WHERE ZJID = cs.ZCONTACTIDENTIFIER),
            -- Tier 3: JID.
            m.ZFROMJID
        )
    END                                                        AS sender_name,
    COALESCE(gm.ZMEMBERJID, m.ZFROMJID)                        AS sender_jid,
    m.ZMESSAGETYPE                                             AS message_type,
    CASE m.ZMESSAGETYPE
        WHEN 0  THEN 'text'
        WHEN 1  THEN 'image'
        WHEN 2  THEN 'video'
        WHEN 3  THEN 'audio'
        WHEN 4  THEN 'contact'
        WHEN 5  THEN 'location'
        WHEN 7  THEN 'link'
        WHEN 8  THEN 'document'
        WHEN 10 THEN 'call'
        WHEN 11 THEN 'system'
        WHEN 14 THEN 'deleted'
        WHEN 15 THEN 'sticker'
        ELSE 'other'
    END                                                        AS message_type_name,
    m.ZTEXT                                                    AS text,
    m.ZPARENTMESSAGE                                           AS reply_to_id,
    m.ZSTANZAID                                                AS stanza_id,
    -- Link preview metadata (type 7 only; NULL for everything else).
    mi.ZMEDIAURL                                               AS link_url,
    mi.ZTITLE                                                  AS link_title
FROM ZWAMESSAGE m
LEFT JOIN ZWACHATSESSION cs ON cs.Z_PK = m.ZCHATSESSION
LEFT JOIN ZWAGROUPMEMBER  gm ON gm.Z_PK = m.ZGROUPMEMBER
LEFT JOIN ZWAMEDIAITEM    mi ON mi.ZMESSAGE = m.Z_PK AND m.ZMESSAGETYPE = 7;


-- ----------------------------------------------------------------------------
-- People tagging (GUI-written, agent-read). The user identifies people in
-- the app's People view (on-device face clustering + naming); those tags
-- are stored here as sidecar tables and carried forward across re-syncs by
-- mergeSidecarsForward (like wa_image_text / wa_voice_text), so naming work
-- is never lost. CREATE IF NOT EXISTS so re-applying views.sql never wipes
-- existing tags; the canonical DDL also lives in sidecar.go's
-- createPersonSidecarsSQL — keep the two in sync.
--
--   wa_person       — one row per identified person (name is lowercase &
--                     unique: same name = same person). hidden=1 removes a
--                     group from the grid + from v_person_photo (junk).
--   wa_person_face  — which detected face (message rowid + face index)
--                     belongs to which person. rowid = ZWAMESSAGE.Z_PK.
-- ----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS wa_person (
    person_id  INTEGER PRIMARY KEY,
    name       TEXT NOT NULL DEFAULT '',
    hidden     INTEGER NOT NULL DEFAULT 0,
    updated_at TEXT
);
CREATE INDEX IF NOT EXISTS wa_person_name_idx ON wa_person(name);
CREATE TABLE IF NOT EXISTS wa_person_face (
    rowid     INTEGER NOT NULL,
    face_idx  INTEGER NOT NULL,
    person_id INTEGER NOT NULL,
    PRIMARY KEY (rowid, face_idx)
);
CREATE INDEX IF NOT EXISTS wa_person_face_person_idx ON wa_person_face(person_id);


-- ----------------------------------------------------------------------------
-- v_person_photo: the agent's "show me photos of <name>" surface. One row
-- per (named, non-hidden) person × photo they appear in. Names are
-- lowercase — match case-insensitively. `image_path` is a hint; the real
-- extension may differ (see ./media/), so prefer `open ./media/<rowid>.*`.
-- ----------------------------------------------------------------------------
DROP VIEW IF EXISTS v_person_photo;
CREATE VIEW v_person_photo AS
SELECT DISTINCT
    p.name                                    AS person,
    m.rowid                                   AS rowid,
    m.ts                                      AS ts,
    m.chat_title                              AS chat_title,
    m.sender_name                             AS sender_name,
    './media/' || m.rowid || '.jpg'           AS image_path
FROM   wa_person_face pf
JOIN   wa_person      p ON p.person_id = pf.person_id AND p.name <> '' AND p.hidden = 0
JOIN   v_messages     m ON m.rowid = pf.rowid;


-- ----------------------------------------------------------------------------
-- messages_fts: FTS5 full-text index over message text.
--   - rowid joins back to ZWAMESSAGE.Z_PK (i.e. v_messages.rowid).
--   - Use MATCH for queries: SELECT rowid, snippet(messages_fts, 0, '[[', ']]', '...', 12)
--                            FROM messages_fts WHERE messages_fts MATCH 'foo NEAR bar';
--   - Tokenizer: unicode61 with diacritic-folding, so "cafe" matches "café".
--
-- This script only DEFINES the (empty) virtual table. The population step
-- is intentionally NOT in SQL: Go's RebuildFTS() builds a SELECT that
-- LEFT-JOINs wa_image_text / wa_voice_text / wa_document into the indexed
-- text when those sidecar tables exist, so that media-index / voice-index
-- runs automatically extend the FTS reach without needing a separate
-- migration step.
-- ----------------------------------------------------------------------------
DROP TABLE IF EXISTS messages_fts;
CREATE VIRTUAL TABLE messages_fts USING fts5(
    text,
    tokenize = 'unicode61 remove_diacritics 2'
);
