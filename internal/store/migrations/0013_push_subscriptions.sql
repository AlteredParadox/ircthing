-- 0013_push_subscriptions: Web Push endpoints registered by the user's
-- browsers (RFC 8030 subscriptions). One row per browser/device profile;
-- the endpoint URL is the identity (re-subscribing refreshes keys in
-- place). p256dh/auth are the client's RFC 8291 keys, base64url. The
-- user_id FK matches the schema's bouncer model even though the app runs
-- single-user today (users row 1 seeded by 0001_initial).

CREATE TABLE push_subscriptions (
    id           INTEGER PRIMARY KEY,
    user_id      INTEGER NOT NULL DEFAULT 1 REFERENCES users(id) ON DELETE CASCADE,
    endpoint     TEXT NOT NULL UNIQUE,
    p256dh       TEXT NOT NULL,
    auth         TEXT NOT NULL,
    created_at   INTEGER NOT NULL,
    last_success INTEGER NOT NULL DEFAULT 0
) STRICT;
