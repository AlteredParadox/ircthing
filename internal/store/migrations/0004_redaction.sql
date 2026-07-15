-- 0004_redaction: mark messages deleted via draft/message-redaction.
--
-- A redaction flags an existing message (found by its per-buffer msgid)
-- rather than removing it: the spec recommends keeping visible redaction
-- history, so the row stays and the client renders a tombstone. The
-- optional reason is stored for display. Redacted messages are excluded
-- from full-text search.

ALTER TABLE messages ADD COLUMN redacted INTEGER NOT NULL DEFAULT 0;
ALTER TABLE messages ADD COLUMN redact_reason TEXT;
