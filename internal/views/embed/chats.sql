-- chats.sql
-- v_chats and the read state behind its unread column. Separate from
-- views.sql because live has to apply this to a running workspace,
-- where views.sql (which drops the FTS index) must not run. Idempotent.

-- ----------------------------------------------------------------------------
-- wa_chat_read: how far each chat has been read, as live hears it from the
-- user's own devices (read receipts, "Mark as read/unread"). Times are Cocoa
-- seconds, like ZMESSAGEDATE.
--   - read_through:  incoming messages up to here are read; NULL = none.
--   - unread_marked: when the chat was marked unread; NULL = not marked.
--                    A read at or after that time clears it.
-- Seeded once, when the table is created: every chat read through that
-- moment (EnsureChats). The phone's own ZUNREADCOUNT is not used — it is
-- only as fresh as the backup.
-- ----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS wa_chat_read (
    chat_pk        INTEGER PRIMARY KEY,
    read_through   REAL,
    unread_marked  REAL
);

-- v_chats.unread counts a chat's messages after a point in time.
CREATE INDEX IF NOT EXISTS wa_message_chat_date_idx ON ZWAMESSAGE(ZCHATSESSION, ZMESSAGEDATE);

-- ----------------------------------------------------------------------------
-- v_chats: one row per chat session (1:1 or group).
--   - Cocoa epoch (seconds since 2001-01-01 UTC) converted to UTC ISO.
--   - kind: 'group' if the JID ends in @g.us, otherwise 'dm'.
--   - unread: incoming messages after the chat was last read — not
--     system rows (6) or deleted ones (14) — and at least 1 while the
--     chat is marked unread. 0 means read.
-- ----------------------------------------------------------------------------
DROP VIEW IF EXISTS v_chats;
CREATE VIEW v_chats AS
SELECT
    cs.Z_PK                                                    AS chat_id,
    cs.ZCONTACTJID                                             AS jid,
    -- For DM chats, resolve the partner's display name in this strict
    -- preference order (the USER's saved name wins over the partner's
    -- self-chosen name; opaque identifiers are last resort):
    --   1. iOS Contacts label (wa_contact).
    --   2. ZPARTNERNAME, but ONLY when it's a real name — WhatsApp
    --      mirrors iOS Contacts here, but falls back to a formatted
    --      phone string ("+971 50 ...") for unsaved contacts; skip those.
    --   3. WhatsApp push name keyed by the phone JID.
    --   4. WhatsApp push name keyed by the partner's LID alias.
    --   5. Raw JID (stable, joinable) as last resort.
    -- For groups, just the group title.
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
    cs.ZARCHIVED                                               AS archived,
    MAX(
        (SELECT COUNT(*) FROM ZWAMESSAGE m
          WHERE m.ZCHATSESSION = cs.Z_PK
            AND m.ZMESSAGEDATE > COALESCE(r.read_through, 0)
            AND COALESCE(m.ZISFROMME, 0) = 0
            AND COALESCE(m.ZMESSAGETYPE, 0) NOT IN (6, 14)),
        r.unread_marked IS NOT NULL
    )                                                          AS unread
FROM ZWACHATSESSION cs
LEFT JOIN wa_chat_read r ON r.chat_pk = cs.Z_PK;
