-- 0012_archived_buffers: non-destructive close. close_buffer with
-- purge:false marks the buffer archived instead of deleting it: hidden
-- from the sidebar (Buffers/RecentBuffers filter on this flag) with its
-- messages, read marker, and FTS rows intact. Real conversation — or our
-- own live rejoin of the channel — clears the flag and the buffer
-- resurfaces with its scrollback; membership fan-out
-- (JOIN/PART/QUIT/NICK/MODE) never does, which is what keeps the PART
-- echo racing a purge:false close from resurrecting the buffer (the
-- deleted path's close tombstone plays that role for purge:true).
ALTER TABLE buffers ADD COLUMN archived INTEGER NOT NULL DEFAULT 0;
