-- 0011_fts_recreate: undo 0010's over-indexing without leaving NULL-text orphans.
--
-- 0010's unconditional 'rebuild' indexed EVERY messages row, including the
-- (usually majority) system rows — JOIN/PART/MODE/QUIT/... — whose text is
-- NULL/empty. The 0003 triggers deliberately skip those, so their tokenless
-- %_docsize entries are never removed by the conditional delete trigger; as
-- system rows are pruned or cascade-deleted their docsize entries leak,
-- bloating the file (a one-time seed on databases that held system rows when
-- 0010 ran; fresh databases rebuilt an empty table and are unaffected).
--
-- Rebuild the index the way the triggers maintain it — text-bearing rows
-- only. DROP frees the old FTS segments (including the redacted-token
-- remnants 0010 aimed to clear), which core secure_delete (DSN pragma)
-- zeroes; the repopulation matches the insert trigger's WHEN clause, so no
-- orphan docsize rows are created. The triggers reference messages_fts by
-- name, so recreating it under the same name keeps them valid.
DROP TABLE messages_fts;

CREATE VIRTUAL TABLE messages_fts USING fts5(
    text,
    content='messages',
    content_rowid='id',
    tokenize='unicode61 remove_diacritics 2'
);

INSERT INTO messages_fts(messages_fts, rank) VALUES('secure-delete', 1);

INSERT INTO messages_fts(rowid, text)
SELECT id, text FROM messages WHERE text IS NOT NULL AND text <> '';
