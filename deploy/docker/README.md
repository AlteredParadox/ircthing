# ircthing in Docker

A turnkey **ircthing + Caddy** stack: Caddy handles automatic HTTPS and
reverse-proxies to ircthing, which stays on a private network. This is the
recommended container deployment — it keeps each process in its own container
(the thing Docker is good at) instead of cramming a web server, the app, and a
firewall daemon into one image.

> **Not using Docker?** The single static binary + the systemd unit in
> `deploy/ircthing.service` remains the simplest deployment. Docker is just an
> alternative packaging.

## What's here

| File | Purpose |
|------|---------|
| `../../Dockerfile` | Multi-stage build → minimal Alpine image with the static binary |
| `docker-compose.yml` | The ircthing + Caddy stack |
| `Caddyfile` | Automatic-HTTPS reverse proxy → `ircthing:8067` |
| `config.example.json` | Container-flavored config (listens on `0.0.0.0`, DB under the volume) |
| `.env.example` | Domain + ACME email for Caddy |
| `fail2ban-jail.local` | Host-side fail2ban jail for the containerized deployment |

## Quick start

From this directory:

```sh
cp .env.example .env                 # set IRCTHING_DOMAIN + ACME_EMAIL
cp config.example.json config.json   # your networks live here

# Generate the login password hash with the image itself (-it: the prompt
# reads from an interactive terminal — without it stdin is closed and the
# read fails):
docker compose build ircthing
docker run --rm -it ircthing:local -hash-password
# paste the hash into config.json -> user.password_hash, and finish editing
# your networks now, WHILE you still own the file.

# The config holds credentials (SASL, proxy, WireGuard keys). Once editing is
# done, restrict it to root + the container user (uid 10001) so other host
# users cannot read it. Both commands need sudo — after the chown you no
# longer own the file. (The read-only bind mount needs it readable by uid
# 10001, not world, hence 0600 owned by that uid; to edit it again later,
# `sudo` the editor or chown it back to yourself first.)
sudo chown 10001:10001 config.json && sudo chmod 0600 config.json

docker compose up -d
docker compose logs -f ircthing
```

Point `IRCTHING_DOMAIN` at this host with ports **80** and **443** reachable
from the internet, and Caddy fetches a Let's Encrypt certificate on first
request. For a purely local test, set `IRCTHING_DOMAIN=localhost` — Caddy then
serves its own internal CA cert (the browser warning is expected).

> **IPv4 only.** The published ports bind `0.0.0.0` (IPv4) deliberately — see
> [How it fits together](#how-it-fits-together) for why. Give the domain an
> **A record only, no AAAA**: an AAAA record would advertise an address the
> stack doesn't serve (breaking IPv6 clients, and ACME may prefer IPv6). Add
> AAAA only after enabling IPv6 on the bridge and verifying real-client source
> logging and fail2ban bans over IPv6.

## Prebuilt image (GHCR)

Every `vX.Y.Z` release publishes a multi-arch (amd64 + arm64) image to
`ghcr.io/alteredparadox/ircthing` (built by
[`.github/workflows/docker-release.yml`](../../.github/workflows/docker-release.yml)).
To use it instead of building locally, edit `docker-compose.yml`:

```yaml
  ircthing:
    image: ghcr.io/alteredparadox/ircthing:latest   # or pin :0.2.4
    # delete the `build:` block
```

then `docker compose pull && docker compose up -d`. Or run it directly:

```sh
docker run --rm -it ghcr.io/alteredparadox/ircthing:latest -hash-password
```

## How it fits together

```
          :80 / :443                internal network
client ───────────────▶ Caddy ───────────────────────▶ ircthing :8067
                     (TLS, ACME)   X-Forwarded-For       (behind_proxy=true)
```

- **ircthing is never published to the host** — it has no `ports:` mapping, so
  the only path in is through Caddy over TLS.
- Because every request arrives via Caddy, `config.json` **must** set
  `behind_proxy: true`. Caddy appends the real client to `X-Forwarded-For`, and
  ircthing uses that for login rate-limiting and for the fail2ban log lines. If
  you forget it, every client (and every attacker) shares Caddy's address and
  the rate-limiter can lock everyone out at once.
- WebSockets (`/api/ws`) upgrade through Caddy transparently — no extra config.

## Data & backups

Three named volumes hold everything stateful:

- `ircthing_data` → `/var/lib/ircthing` — the SQLite database (scrollback,
  networks, read markers, push subscriptions).
- `caddy_data` → `/data` — the ACME account key and issued certificates.
  **Persist this** so you don't hit Let's Encrypt rate limits on every redeploy.
- `caddy_config` → `/config` — Caddy's autosaved runtime config.

The database runs in **WAL mode**, so copying the `.db` file alone can miss
committed data still in the `-wal` sidecar or race a checkpoint. Take a
consistent snapshot by stopping ircthing (a clean shutdown quiesces SQLite),
copying the whole directory including sidecars, then restarting. Keep backups
**outside this checkout** (they contain credentials — a `config.json.bak` in
the repo could be committed or sent to a builder) in a root-only directory:

```sh
sudo mkdir -p /var/backups/ircthing && sudo chmod 700 /var/backups/ircthing
docker compose stop ircthing
sudo docker compose cp ircthing:/var/lib/ircthing /var/backups/ircthing/data  # db + -wal + -shm; sudo to write the root-only dir
docker compose start ircthing
sudo cp config.json /var/backups/ircthing/config.json.bak                     # credentials (needs sudo: 0600 / uid 10001)
```

**Disk:** the DB is not memory-bounded and grows with scrollback. Set
`retention_days` / `retention_max_messages` in `config.json`, and remember the
volume lives under Docker's data root (`/var/lib/docker` by default) — monitor
that filesystem or put it on one with a quota.

## Upgrading

```sh
git pull
docker compose up -d --build   # rebuilds the image, restarts, keeps volumes
```

To stamp a proper version string into the About panel, build via the Makefile
target from the repo root instead — it passes `git describe` as the build arg:

```sh
make docker            # builds ircthing:local with VERSION=$(git describe ...)
docker compose up -d   # (no --build; reuse the stamped image)
```

## Banning brute-force logins (fail2ban on the host)

fail2ban stays on the **host**, not in a container — a firewall rule applied
inside a container's network namespace doesn't protect the host, and giving a
container `NET_ADMIN` to let it try would undercut the isolation that's the
whole point. The host watches the container's journal and bans at the host
firewall.

1. Install the shared filter and this jail:

   ```sh
   sudo cp ../fail2ban/ircthing.conf /etc/fail2ban/filter.d/ircthing.conf
   sudo cp fail2ban-jail.local        /etc/fail2ban/jail.d/ircthing-docker.conf
   sudo systemctl restart fail2ban
   sudo fail2ban-client status ircthing-docker
   ```

2. The compose file already logs ircthing to journald with
   `container_name: ircthing`, which is how the jail's
   `journalmatch = CONTAINER_NAME=ircthing` finds the lines.

**The Docker gotcha:** published container ports are DNAT'd and traverse the
`DOCKER-USER` iptables chain, *bypassing* `INPUT`. fail2ban's default action
inserts into `INPUT`, so a ban would silently fail to block anything. The jail
here overrides the action to insert into `DOCKER-USER`
(`iptables-allports[chain=DOCKER-USER]`), which Docker guarantees is evaluated
before its own forwarding rules — so the ban actually takes effect. On a
pure-nftables host without the iptables-nft shim, use the `nftables-allports`
action and add a hook into Docker's forward path instead.

Verify the whole path end-to-end after starting the stack: attempt a few bad
logins, then

```sh
journalctl -o cat CONTAINER_NAME=ircthing --no-pager | grep '^login:'
sudo fail2ban-client status ircthing-docker
```
