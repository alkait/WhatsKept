-- views.sql
-- Convenience views + FTS5 index applied to a freshly extracted ChatStorage.sqlite.
-- This file is idempotent: re-applying it drops and recreates everything it owns.
-- Read-only against the underlying Z* tables; never modifies them.

PRAGMA foreign_keys = OFF;

-- ----------------------------------------------------------------------------
-- wa_contact: iOS-Contacts mapping (populated by the contacts sync).
--   - jid:           '<digits>@s.whatsapp.net'
--   - display_name:  the user's chosen label from their iOS Contacts app.
--   - source:        provenance, currently always 'ios-contacts'.
-- CREATE IF NOT EXISTS so v_messages can LEFT JOIN it unconditionally,
-- whether or not the contacts sync has run yet.
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
-- user's saved iOS-Contacts entries are keyed by phone JID. The bridge is
-- ZWACHATSESSION: every 1:1 chat session with a partner has both
-- ZCONTACTJID (phone) and ZCONTACTIDENTIFIER (LID).
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
-- Purely derived metadata — repopulated every time this script runs. The
-- filename alone makes "did Khalid send me that passport scan?" answerable
-- via messages_fts.
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
    -- Extension = substring after the LAST '.'. SQLite has no rfind:
    -- RTRIM the path against (the path with all dots removed) — RTRIM
    -- stops at the first dot from the right, leaving everything up to
    -- and including that dot; SUBSTR takes the tail. NULL when the
    -- path has no dot at all.
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
-- v_messages: flattened message view.
--   - ts: UTC ISO timestamp (Cocoa epoch already converted).
--   - sender_name: best-effort; 'me' for outgoing. See tier comments below.
--   - reply_to_id: FK back to v_messages.rowid for quoted-reply chains.
--   - text: ZWAMESSAGE.ZTEXT verbatim. NULL for most media messages.
--   - link_url / link_title: populated for 'link' messages (type 7).
-- ----------------------------------------------------------------------------
DROP VIEW IF EXISTS v_messages;
CREATE VIEW v_messages AS
SELECT
    m.Z_PK                                                     AS rowid,
    m.ZCHATSESSION                                             AS chat_id,
    -- chat_title mirrors v_chats.title — see that view's comment (chats.sql).
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
    -- sender_name resolution, tiered so the USER's saved name always
    -- wins over the sender's self-chosen WhatsApp name:
    --   Tier 1 — saved (the user's address book): wa_contact directly,
    --     wa_contact via the LID→phone bridge, WhatsApp's per-member
    --     iOS-Contacts mirror (groups), or a non-phone-string
    --     ZPARTNERNAME (DMs).
    --   Tier 2 — push name (ZWAPROFILEPUSHNAME), keyed by phone JID or
    --     LID alias. NOT ZWAMESSAGE.ZPUSHNAME — that column holds opaque
    --     protobuf bytes in current iOS WhatsApp builds.
    --   Tier 3 — raw JID.
    -- 'me' short-circuits everything for outgoing messages.
    CASE
        WHEN m.ZISFROMME = 1 THEN 'me'
        WHEN gm.ZMEMBERJID IS NOT NULL THEN COALESCE(
            (SELECT display_name FROM wa_contact         WHERE jid  = gm.ZMEMBERJID),
            (SELECT wc.display_name
               FROM wa_contact wc
               JOIN wa_jid_alias a ON a.phone_jid = wc.jid
              WHERE a.lid_jid = gm.ZMEMBERJID),
            NULLIF(gm.ZCONTACTNAME, ''),
            NULLIF(gm.ZFIRSTNAME,   ''),
            (SELECT ZPUSHNAME    FROM ZWAPROFILEPUSHNAME WHERE ZJID = gm.ZMEMBERJID),
            (SELECT p.ZPUSHNAME
               FROM ZWAPROFILEPUSHNAME p
               JOIN wa_jid_alias a ON a.phone_jid = p.ZJID
              WHERE a.lid_jid = gm.ZMEMBERJID),
            gm.ZMEMBERJID
        )
        ELSE COALESCE(
            (SELECT display_name FROM wa_contact         WHERE jid  = m.ZFROMJID),
            (SELECT wc.display_name
               FROM wa_contact wc
               JOIN wa_jid_alias a ON a.phone_jid = wc.jid
              WHERE a.lid_jid = m.ZFROMJID),
            CASE
                WHEN cs.ZPARTNERNAME IS NULL OR cs.ZPARTNERNAME = '' THEN NULL
                WHEN cs.ZPARTNERNAME GLOB '+[0-9 ]*'                 THEN NULL
                ELSE cs.ZPARTNERNAME
            END,
            (SELECT ZPUSHNAME    FROM ZWAPROFILEPUSHNAME WHERE ZJID = m.ZFROMJID),
            (SELECT ZPUSHNAME    FROM ZWAPROFILEPUSHNAME WHERE ZJID = cs.ZCONTACTIDENTIFIER),
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
        WHEN 11 THEN 'gif'
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
-- messages_fts: FTS5 full-text index over message text.
--   - rowid joins back to ZWAMESSAGE.Z_PK (i.e. v_messages.rowid).
--   - Tokenizer: unicode61 with diacritic-folding, so "cafe" matches "café".
--
-- This script only DEFINES the (empty) virtual table. Population happens
-- in Go (rebuildFTS), which LEFT-JOINs the enrichment tables
-- (wa_image_text / wa_voice_text / wa_document_text) into the indexed
-- text when they exist, so enrichment runs extend FTS reach without a
-- separate migration.
-- ----------------------------------------------------------------------------
DROP TABLE IF EXISTS messages_fts;
CREATE VIRTUAL TABLE messages_fts USING fts5(
    text,
    tokenize = 'unicode61 remove_diacritics 2'
);
