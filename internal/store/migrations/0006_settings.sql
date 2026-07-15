-- Generic key-value settings, shared by every client device. First user:
-- the web client's appearance preferences (one JSON blob under 'prefs'),
-- so themes follow the user across devices. Values are opaque to the
-- server; clients validate their own blobs.
CREATE TABLE settings (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
) STRICT;
