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
./bin/ircd-web -hash-password       # type a login password (8–72 chars), copy the hash
cp config.example.json config.json
chmod 600 config.json               # it holds the password hash + IRC/SASL/proxy secrets
$EDITOR config.json                 # set user, networks
./bin/ircd-web -config config.json
```

The config file holds credentials, so keep it `0600` (the systemd unit below
uses a root-owned credential instead).

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
| `retention_days` | Prune stored messages older than this many days. Default 0 (keep forever). Pruning runs hourly and keeps the search index in step. A hot buffer's in-memory ring may briefly still show just-pruned messages until they evict. |
| `retention_max_messages` | Keep at most this many messages per buffer; older ones are pruned. Default 0 (unlimited). Retention is **per buffer**, not an aggregate disk cap: a server that opens many buffers can still grow the database. For production, put the database on a volume with a filesystem quota (or a dedicated volume) and monitor disk — the systemd unit bounds memory, not disk. Editable live in **Settings → History retention** (the stored value then wins over this seed). |
| `behind_proxy` | Set `true` when a **trusted single-hop** reverse proxy fronts the binary. The login rate-limit then keys on the **last `X-Forwarded-For` entry** (the hop the proxy appends) instead of the shared proxy address, so one attacker can't lock out everyone. The proxy **must** append the real client to `X-Forwarded-For` — Caddy's `reverse_proxy` does this by default; for nginx set `proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;`. `X-Real-IP` is deliberately **ignored** (Caddy forwards a client-set `X-Real-IP` unchanged by default, so trusting it would let an attacker spoof the backoff key). Leave `false` for direct/loopback deployments — otherwise a client could spoof `X-Forwarded-For` to evade the backoff. It also lets the WebSocket origin check verify the request scheme via `X-Forwarded-Proto` (both Caddy and nginx send it) rather than falling back to a host-only check; with it off behind a TLS-terminating proxy the check still permits the connection, just without the scheme assertion. |
| `disable_previews` | **Initial default** for the previews switch (tri-state). **Omit it and previews start OFF** — the privacy-first default, since an auto-fetched preview is a tracking beacon (a poster learns when a buffer is viewed) and the server makes **zero** outbound fetches. Set it to `false` to start with previews **on**, or `true` to be explicit about off. Toggle it live in **Settings → Link previews** (the saved value wins over this). Previews are fetched through **each link's own network proxy** — see [Preview fetches & the proxy SSRF caveat](#preview-fetches--the-proxy-ssrf-caveat). |

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
| `proxy` | `socks5://[user:pass@]host:port` (DNS resolves proxy-side) or `http://host:port` (CONNECT tunnel). Proxy auth is transmitted **in cleartext** (SOCKS5 username/password per RFC 1929, HTTP Basic), so only use credentialed proxies whose transport is itself encrypted or local/trusted (e.g. loopback, or inside a VPN tunnel). Mutually exclusive with `wireguard`. |
| `wireguard` | Egress this network through an in-process userspace WireGuard tunnel (no TUN device, no root) instead of a proxy — its Noise handshake authenticates without the cleartext exposure `proxy` auth has. Object with `private_key`, `peer_public_key`, `endpoint` (`host:port`; a hostname is resolved locally, pre-tunnel), `address` (this client's address inside the tunnel), `dns` (in-tunnel resolver, `ip` or `ip:port`, default `:53`), and optional `preshared_key` / `mtu` (default 1420). Keys are standard WireGuard base64 (as `wg genkey` / Mullvad print them). Target DNS resolves through the tunnel (no local leak). Configurable in the web UI under **Egress → WireGuard tunnel**. Mutually exclusive with `proxy`. |
| `nick`, `username`, `realname` | Identity. `username`/`realname` default to the nick. |
| `pass` | Server password (`PASS`), rarely needed. |
| `sasl` | `mechanism` `""` picks automatically (EXTERNAL without a password, else SCRAM-SHA-256 when offered, else PLAIN). `cert_file`/`key_file` supply the client certificate for EXTERNAL. SCRAM-SHA-256 does **not** apply SASLprep normalization to either the **login (account name) or the password** (RFC 5802 §2.2), so use ASCII (or already-normalized) values for both — a non-ASCII login or password may not match a server that normalizes it. |
| `channels` | Joined after every registration, so they come back on reconnect. The UI keeps this in sync: joining via the network menu adds to it, the *Leave channel* action removes. |

### Preview fetches & the proxy SSRF caveat

Link and image previews are fetched server-side, through the **proxy of the
network the link came from** — a link in a proxied network is previewed over
that proxy (your egress IP never leaks), one in a direct network goes
direct, and if the link's network can't be resolved to a direct-or-proxied
decision the fetch is **refused** rather than sent direct. There is no
separate media proxy to configure.

Direct (unproxied) fetches are hardened: the *resolved* IP of every
connection and redirect hop is checked against a public-address policy at
connect time, which is rebinding-safe. This blocks private, loopback,
link-local, and the **well-known** NAT64 translation prefixes (RFC 6052/8215).
The one residual: a host with a **site-specific** NAT64 prefix (a custom NSP)
could translate a blocked IPv4 destination through that prefix — such prefixes
can't be enumerated statically. If you deploy on a NAT64 network with a custom
NSP, confirm the host has no such route, or leave previews off.

The one nuance is on the **proxied** path: the proxy owns DNS, so the server
can only block *literal* private-IP targets — a hostname that resolves
*proxy-side* to an internal address is reachable through the proxy. Whether
that matters depends on **where the proxy runs**:

- **A commercial VPN's SOCKS5 (Mullvad, TorGuard, …), or Tor — low exposure,
  but the provider is a trust boundary.** The fetch egresses from the
  provider's network, not yours, so it cannot reach your LAN, loopback, or
  cloud metadata; those are exactly what the proxy shields. What remains
  reachable is the provider's own internal infrastructure — reputable
  providers isolate it, but you are trusting them to. (Tor also refuses
  private-IP destinations by exit policy.)
- **A SOCKS proxy on your own machine or LAN** (self-hosted `dante`,
  `ssh -D`, a local daemon) — **the case to watch.** There `127.0.0.1` and
  `192.168.x/10.x` resolve to *your* host/network, so a malicious preview
  link could probe internal services. Use a proxy whose egress you trust, or
  turn previews off (the previews switch in **Settings → Link previews** is
  global — there is no per-network preview toggle).

## Deployment

The listen address stays on loopback by design: put a TLS-terminating
reverse proxy (Caddy, nginx) in front for anything beyond localhost,
and set `"secure_cookies": true` (as in the example config) so the
session cookie is only ever sent over HTTPS — leave it `false` only for
plain-HTTP localhost testing, where a secure cookie would never be sent.
Also enable HSTS at the proxy (e.g. `Strict-Transport-Security:
max-age=63072000; includeSubDomains`) so browsers refuse to downgrade.
WebSocket upgrade for `/api/ws` must be allowed through the proxy (Caddy
does this automatically); rate-limiting `/api/login` at the proxy is also
recommended (the binary applies its own per-source backoff as a second
layer). The proxy must **forward the `Origin` and `Sec-Fetch-Site` request
headers unchanged** — the state-changing/media endpoints use them as a CSRF
defense and fail closed without them (a proxy that strips both would make
those endpoints refuse every request).

A hardened systemd unit ships in [`deploy/ircthing.service`](deploy/ircthing.service).
It uses `DynamicUser=yes` — no service account to create — and hands the
config to the process as a systemd credential, so `/etc/ircthing/config.json`
stays root-owned and the app reads a private, service-only copy from
`$CREDENTIALS_DIRECTORY`. `StateDirectory=` creates `/var/lib/ircthing`
with the right ownership, so set `"database": "/var/lib/ircthing/ircthing.db"`.

If you use **SASL EXTERNAL** (client-certificate auth), the transient
`DynamicUser` account cannot read a root-owned `/etc/ircthing/*.pem` key.
Deliver the cert and key as additional systemd credentials — add
`LoadCredential=` lines for them and set the JSON values to the env-var form
`"cert_file": "$CREDENTIALS_DIRECTORY/client-cert.pem"` /
`"key_file": "$CREDENTIALS_DIRECTORY/client-key.pem"` (ircthing expands
`$VAR`/`${VAR}` in these two paths at load time) — or place them under the
service-owned `StateDirectory` with appropriate ownership. A world-readable
key is not an acceptable workaround.

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
