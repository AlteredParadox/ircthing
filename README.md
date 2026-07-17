# ircthing

A self-hosted, always-connected web IRC client in a single static Go
binary. The binary contains the bouncer core — persistent IRC
connections, SQLite scrollback, multi-device read-marker sync — and
serves its own web frontend. Think "The Lounge, but one small binary":
you run one process, it stays on IRC, and every browser you log in from
picks up exactly where you left off.

No CGO, no runtime dependencies beyond the config file and the SQLite
database it creates. The binary is ~12 MB; the web bundle inside it is
~26 KB gzipped; a working setup with 5 networks and 10k messages of hot
scrollback runs in ~25 MB of RSS.

## Features

- **Bouncer core**: persistent connections with reconnect
  (exponential backoff + jitter), gap-free scrollback catch-up via
  `chathistory` with paginated backfill, and replay to every device.
- **Protocol**: the full ratified IRCv3 set (SASL PLAIN /
  SCRAM-SHA-256 / EXTERNAL, `server-time`, `batch`, `echo-message`,
  `monitor`, STS with persisted policies, WHOX account discovery, bot
  mode, UTF8ONLY, ...) plus the modern drafts: `chathistory`,
  `read-marker`, `typing`, `multiline`, `message-redaction`,
  `no-implicit-names`. CTCP VERSION/PING/TIME/CLIENTINFO are answered;
  DCC is deliberately out of scope.
- **Connectivity**: TLS with client certificates, certificate
  fingerprint pinning for self-signed servers, SOCKS5 (Tor-friendly:
  DNS resolves proxy-side) and HTTP CONNECT proxies per network.
- **Web UI**: virtualized message list (smooth at 50k+ messages),
  full-text search (FTS5), link previews and image thumbnails through a
  server-side proxy, desktop notifications with per-network highlight
  rules, a MONITOR buddy list with live presence, typing indicators,
  and multiline composing.
- **Multi-device**: read markers, unread counts, and appearance
  preferences sync through the server; `draft/read-marker` bridges read
  state to other bouncer clients.
- **Theming**: dark/light/system, accent colors, text size, density,
  message font — plus a raw custom-CSS override. Usable at 360 px wide;
  installable as a PWA.

### Keyboard

| Keys | Action |
|---|---|
| `Ctrl+K` | channel switcher palette (mentions and unread float to the top) |
| `Ctrl+Shift+F` | full-text search |
| `Alt+↑` / `Alt+↓` | previous / next buffer |
| `Alt+Shift+↑` / `Alt+Shift+↓` | previous / next unread buffer |
| `Tab` / `Shift+Tab` | complete nicks, `/commands`, `:emoji:` — repeat to cycle |
| `Shift+Enter` | newline (sent as `draft/multiline` where supported) |

## Quick start

Requires Go ≥ 1.25 and Node (for the frontend build) — build-time only.

```sh
make build                          # builds web assets + bin/ircd-web
./bin/ircd-web -hash-password       # type a login password, copy the hash
cp config.example.json config.json  # then edit: user, networks
./bin/ircd-web -config config.json
```

Open http://127.0.0.1:8067 and log in with the user from the config.

## Configuration

`config.json` is strict JSON — unknown fields are errors, so typos fail
loudly. See `config.example.json` for a complete example.

| Field | Meaning |
|---|---|
| `listen` | HTTP listen address. Default `127.0.0.1:8067` (loopback only — see Deployment). |
| `database` | SQLite path, created on first run. Default `ircthing.db`. Created mode 0600 (it holds plaintext network credentials and message history); an existing group/world-readable file is tightened to 0600 on start. |
| `user.username`, `user.password_hash` | Web login. Generate the bcrypt hash with `ircd-web -hash-password`. |
| `session_ttl_days` | Login cookie lifetime. Default 30. |
| `ring_size` | Hot scrollback kept in memory per buffer. Default 200; older history is read from SQLite. |
| `disable_previews` | **Initial default** for the previews switch. `true` starts with link/image previews off, so the server makes **zero** outbound fetches. Toggle it live in **Settings → Link previews** (the saved value wins over this). Previews are fetched through **each link's own network proxy**, so a link in a proxied network is previewed over that proxy (no egress-IP leak) and one in a direct network goes direct — there's no separate media proxy to configure. |

Networks are managed from the web UI: the **+** button in the sidebar
adds one; clicking a network's name offers *Join channel*, *Edit
network* (rename keeps the scrollback; saving reconnects), and — inside
the edit form — removal, which deletes the network's scrollback too.
Definitions live in the database; the config file's `networks[]` seeds
it on the **first run only** and is ignored once the table has rows, so
it can be left empty when starting fresh.

Per network (`networks[]` seed / edit form):

| Field | Meaning |
|---|---|
| `addr` | `host:port` of the IRC server. |
| `tls` | Use TLS. Plaintext requires the explicit `allow_plaintext: true` opt-in. |
| `trusted_fingerprints` | Hex SHA-256 pins of the server's certificate; a match replaces CA verification (self-signed servers). |
| `proxy` | `socks5://[user:pass@]host:port` (DNS resolves proxy-side) or `http://host:port` (CONNECT tunnel). |
| `nick`, `username`, `realname` | Identity. `username`/`realname` default to the nick. |
| `pass` | Server password (`PASS`), rarely needed. |
| `sasl` | `mechanism` `""` picks automatically (EXTERNAL without a password, else SCRAM-SHA-256 when offered, else PLAIN). `cert_file`/`key_file` supply the client certificate for EXTERNAL. |
| `channels` | Joined after every registration, so they come back on reconnect. The UI keeps this in sync: joining via the network menu adds to it, the *Leave channel* action removes. |

## Deployment

The listen address stays on loopback by design: put a TLS-terminating
reverse proxy (Caddy, nginx) in front for anything beyond localhost,
and set `"secure_cookies": true` so the session cookie is only ever
sent over HTTPS. WebSocket upgrade for `/api/ws` must be allowed
through the proxy (Caddy does this automatically); rate-limiting
`/api/login` at the proxy is also recommended (the binary applies its
own per-source backoff as a second layer).

A hardened systemd unit ships in [`deploy/ircthing.service`](deploy/ircthing.service).
It uses `DynamicUser=yes` — no service account to create — and hands the
config to the process as a systemd credential, so `/etc/ircthing/config.json`
stays root-owned and the app reads a private, service-only copy from
`$CREDENTIALS_DIRECTORY`. `StateDirectory=` creates `/var/lib/ircthing`
with the right ownership, so set `"database": "/var/lib/ircthing/ircthing.db"`.

```sh
sudo cp bin/ircd-web /usr/local/bin/
sudo mkdir -p /etc/ircthing && sudo cp config.json /etc/ircthing/
sudo chown root:root /etc/ircthing/config.json && sudo chmod 600 /etc/ircthing/config.json
sudo cp deploy/ircthing.service /etc/systemd/system/
sudo systemctl enable --now ircthing
```

The unit sets `GOMEMLIMIT=64MiB`, which keeps the Go heap comfortably
inside the project's 72 MB RSS target (`make memcheck` verifies the
5-networks / 50-channels / 10k-messages scenario at ~25 MB).

## Development

```sh
make check        # vet, staticcheck, all tests, frontend build, size gates
make integration  # end-to-end against a real Ergo IRCd (built into .cache/)
make irctest      # irctest's client suite drives our CAP/SASL/TLS/STS handshake
make memcheck     # RSS scenario under GOMEMLIMIT=64MiB, asserted ≤ 72 MB
make build-debug  # unstripped, race-enabled binary for delve
```

Architecture, protocol scope, budgets, and working rules live in
[CLAUDE.md](CLAUDE.md). The short version: `internal/irc` speaks IRC
(one connection manager per network), `internal/store` owns SQLite,
`internal/hub` fans events out to WebSocket sessions, `internal/api`
is HTTP, and `web/` is a Preact frontend bundled by esbuild and
embedded into the binary. Hard budgets: 30 MB binary, 100 KB gzipped
bundle, 72 MB RSS.
