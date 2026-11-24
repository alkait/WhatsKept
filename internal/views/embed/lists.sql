-- lists.sql
-- WhatsApp's chat lists — the filters at the top of the chat list
-- (Unread, Favorites, Groups and the user's own) — as live last heard
-- them from WhatsApp. live rebuilds both tables from WhatsApp at every
-- connect and follows changes after; WhatsApp stays the source of
-- truth. Runs after chats.sql (v_list_chats reads v_chats). Idempotent.

-- ----------------------------------------------------------------------------
-- wa_list: one row per list that exists (deleted lists are dropped).
--   - id:       WhatsApp's list ID.
--   - kind:     custom (the user's own), favorites, unread, groups,
--               community, … — only custom lists can be changed.
--   - shown:    1 if the list is shown as a filter.
--   - position: place among the filters, 0 first; NULL if unplaced.
-- ----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS wa_list (
    id       INTEGER PRIMARY KEY,
    name     TEXT    NOT NULL,
    kind     TEXT    NOT NULL,
    shown    INTEGER NOT NULL,
    position INTEGER
);

-- ----------------------------------------------------------------------------
-- wa_list_chat: the chats in each list. Built-in filters with rules
-- (Unread, Groups) have none.
--   - jid:   the chat as the list names it — a DM by its LID lately,
--            by its phone JID in older lists; a group by its group JID.
--   - phone: a DM's phone JID, when known.
-- ----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS wa_list_chat (
    list_id INTEGER NOT NULL,
    jid     TEXT    NOT NULL,
    phone   TEXT,
    PRIMARY KEY (list_id, jid)
);

-- ----------------------------------------------------------------------------
-- v_lists: one row per list, with how many chats it holds.
-- ----------------------------------------------------------------------------
DROP VIEW IF EXISTS v_lists;
CREATE VIEW v_lists AS
SELECT
    l.id,
    l.name,
    l.kind,
    l.shown,
    l.position,
    (SELECT COUNT(*) FROM wa_list_chat c WHERE c.list_id = l.id) AS chats
FROM wa_list l;

-- ----------------------------------------------------------------------------
-- v_list_chats: one row per chat in a list.
--   - chat_id/jid/title: the chat as v_chats has it; chat_id is NULL for
--     a chat the database doesn't hold yet (jid and title then fall back
--     to its phone JID, else the list's own JID).
-- ----------------------------------------------------------------------------
DROP VIEW IF EXISTS v_list_chats;
CREATE VIEW v_list_chats AS
SELECT
    l.id                                 AS list_id,
    l.name                               AS list,
    ch.chat_id,
    COALESCE(ch.jid, c.phone, c.jid)     AS jid,
    COALESCE(ch.title, c.phone, c.jid)   AS title
FROM wa_list_chat c
JOIN wa_list l ON l.id = c.list_id
LEFT JOIN v_chats ch ON ch.chat_id = (
    SELECT cs.Z_PK FROM ZWACHATSESSION cs
     WHERE cs.ZCONTACTJID = c.jid
        OR (c.phone IS NOT NULL AND cs.ZCONTACTJID = c.phone)
        OR cs.ZCONTACTIDENTIFIER = c.jid
     ORDER BY cs.Z_PK
     LIMIT 1);
