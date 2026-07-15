-- 0002_unique_msgid: msgids are unique per buffer, making appends
-- idempotent — chathistory backfill can overlap already-stored history
-- and INSERT OR IGNORE deduplicates. Existing duplicates (none expected
-- pre-release, but dev databases exist) are collapsed to the oldest row
-- first. The old non-unique msgid lookup index is superseded.

DELETE FROM messages
WHERE msgid IS NOT NULL
  AND id NOT IN (
    SELECT MIN(id) FROM messages WHERE msgid IS NOT NULL
    GROUP BY buffer_id, msgid
  );

DROP INDEX messages_by_msgid;

CREATE UNIQUE INDEX messages_unique_msgid
    ON messages (buffer_id, msgid) WHERE msgid IS NOT NULL;
