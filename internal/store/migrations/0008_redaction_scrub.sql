-- 0008_redaction_scrub: make redaction destructive (privacy).
--
-- Through 0004 a redaction only flagged the row: the message body stayed
-- in messages.raw / messages.text and its FTS entry remained, so deleted
-- content was still recoverable from the database, backups, and search.
-- Redaction is now a scrub — the row is kept only as a tombstone
-- (sender/time/command + redacted flag + reason). This migration applies
-- the scrub to rows already redacted under the old behavior.
--
-- Order matters: purge the FTS entry (external-content FTS has no update
-- trigger, so it must be told the old text explicitly) BEFORE blanking the
-- body, otherwise the inverted index keeps the redacted tokens.

INSERT INTO messages_fts (messages_fts, rowid, text)
SELECT 'delete', id, text
FROM messages
WHERE redacted = 1 AND text IS NOT NULL AND text <> '';

-- raw is NOT NULL, so blank it rather than nulling; text is nullable.
UPDATE messages SET raw = '', text = NULL WHERE redacted = 1;
