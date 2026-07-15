-- 0001_initial: users, networks, buffers, messages, read markers.
--
-- Multi-user model (bouncer-style): each user owns their networks — an
-- IRC connection has one nick and one identity, so sharing a network
-- between users is not a thing. Everything below networks (buffers,
-- messages, read markers) is user-scoped transitively and needs no user
-- column of its own. In particular a read marker stays one row per
-- buffer, shared by all of that user's devices — that sharing is exactly
-- what multi-device read sync means.
--
-- The application currently runs single-user: this migration seeds user 1
-- and the store pins it until auth lands in internal/api. Auth columns
-- (password hash, etc.) arrive with that work as a new migration.
--
-- A buffer is one (network, target) pair — a channel, a query, or later a
-- server buffer. messages_by_buffer_time is therefore the
-- (network, target, timestamp) index required by CLAUDE.md, with id as the
-- tiebreaker for stable ordering of same-millisecond messages.
--
-- Timestamps are integer unix milliseconds (server-time precision, no
-- timezone ambiguity).

CREATE TABLE users (
    id       INTEGER PRIMARY KEY,
    username TEXT NOT NULL UNIQUE
) STRICT;

INSERT INTO users (id, username) VALUES (1, 'default');

CREATE TABLE networks (
    id      INTEGER PRIMARY KEY,
    user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name    TEXT NOT NULL,
    UNIQUE (user_id, name)
) STRICT;

CREATE TABLE buffers (
    id         INTEGER PRIMARY KEY,
    network_id INTEGER NOT NULL REFERENCES networks(id) ON DELETE CASCADE,
    name       TEXT NOT NULL,
    UNIQUE (network_id, name)
) STRICT;

CREATE TABLE messages (
    id        INTEGER PRIMARY KEY,
    buffer_id INTEGER NOT NULL REFERENCES buffers(id) ON DELETE CASCADE,
    ts        INTEGER NOT NULL, -- unix ms; server-time tag when present
    msgid     TEXT,             -- IRCv3 msgid tag, NULL when absent
    sender    TEXT NOT NULL,    -- message prefix name (nick or server)
    command   TEXT NOT NULL,    -- PRIVMSG, NOTICE, JOIN, ...
    raw       TEXT NOT NULL     -- full IRC line including tags
) STRICT;

CREATE INDEX messages_by_buffer_time ON messages (buffer_id, ts, id);
CREATE INDEX messages_by_msgid ON messages (msgid) WHERE msgid IS NOT NULL;

CREATE TABLE read_markers (
    buffer_id INTEGER PRIMARY KEY REFERENCES buffers(id) ON DELETE CASCADE,
    ts        INTEGER NOT NULL -- unix ms of the newest read message
) STRICT;
