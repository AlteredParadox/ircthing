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

# Generate the login password hash with the image itself:
docker compose build ircthing
docker run --rm ircthing:local -hash-password
# paste the hash into config.json -> user.password_hash

docker compose up -d
docker compose logs -f ircthing
```

Point `IRCTHING_DOMAIN` at this host with ports **80** and **443** reachable
from the internet, and Caddy fetches a Let's Encrypt certificate on first
request. For a purely local test, set `IRCTHING_DOMAIN=localhost` — Caddy then
serves its own internal CA cert (the browser warning is expected).

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
docker run --rm ghcr.io/alteredparadox/ircthing:latest -hash-password
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

Two named volumes hold everything stateful:

- `ircthing_data` → `/var/lib/ircthing` — the SQLite database (scrollback,
  networks, read markers, push subscriptions).
- `caddy_data` → `/data` — the ACME account key and issued certificates.
  **Persist this** so you don't hit Let's Encrypt rate limits on every redeploy.

Back up the SQLite DB with a consistent snapshot:

```sh
docker compose exec ircthing sh -c \
  'cd /var/lib/ircthing && cp ircthing.db /var/lib/ircthing/backup.db'
docker compose cp ircthing:/var/lib/ircthing/backup.db ./ircthing-backup.db
```

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
journalctl CONTAINER_NAME=ircthing --no-pager | grep '^login:'
sudo fail2ban-client status ircthing-docker
```
