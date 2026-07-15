-- 0003_fts: full-text search over message bodies (FTS5).
--
-- messages.text holds the extracted, searchable body (the PRIVMSG/NOTICE
-- content, CTCP ACTION unwrapped) — set by the hub, which owns IRC
-- parsing; system lines (JOIN/PART/MODE/...) and non-ACTION CTCP get NULL
-- and are not indexed. The raw line stays the source of truth for
-- rendering; text is only the search extract.
--
-- messages_fts is an external-content index (content='messages'): it
-- stores the inverted index but reads column values from messages, so the
-- body is not duplicated a third time. Triggers keep it in sync;
-- messages.text is immutable once written (appends are INSERT OR IGNORE,
-- never UPDATE), so only insert and delete triggers are needed. Delete
-- fires on buffer/network cascade so search never returns purged rows.
--
-- Pre-existing rows (dev databases) have NULL text and are not
-- backfilled — IRC bodies can't be re-extracted in SQL — so only messages
-- stored from here on are searchable. Acceptable pre-release.

ALTER TABLE messages ADD COLUMN text TEXT;

CREATE VIRTUAL TABLE messages_fts USING fts5(
    text,
    content='messages',
    content_rowid='id',
    tokenize='unicode61 remove_diacritics 2'
);

CREATE TRIGGER messages_fts_ai AFTER INSERT ON messages
WHEN new.text IS NOT NULL AND new.text <> '' BEGIN
    INSERT INTO messages_fts (rowid, text) VALUES (new.id, new.text);
END;

CREATE TRIGGER messages_fts_ad AFTER DELETE ON messages
WHEN old.text IS NOT NULL AND old.text <> '' BEGIN
    INSERT INTO messages_fts (messages_fts, rowid, text) VALUES ('delete', old.id, old.text);
END;
