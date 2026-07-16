-- 0007_network_configs: network definitions managed from the web UI.
--
-- config holds one JSON netconf.Network per row. The config file's
-- networks[] seeds this table when it is empty (first run); afterwards
-- the database is the source of truth and the file list is ignored.
-- Secrets (server password, SASL password) are stored as-is: the
-- database sits at the same trust level as the config file next to it.

CREATE TABLE network_configs (
    name   TEXT PRIMARY KEY,
    config TEXT NOT NULL
) STRICT;
