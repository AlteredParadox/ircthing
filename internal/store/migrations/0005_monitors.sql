-- 0005_monitors: the MONITOR buddy list (nicks whose online/offline
-- presence we track), persisted per network so it survives reconnects and
-- restarts. Online/offline status itself is ephemeral (from RPL_MONONLINE
-- / RPL_MONOFFLINE) and is not stored.

CREATE TABLE monitors (
    id         INTEGER PRIMARY KEY,
    network_id INTEGER NOT NULL REFERENCES networks(id) ON DELETE CASCADE,
    nick       TEXT NOT NULL,
    UNIQUE (network_id, nick)
) STRICT;
