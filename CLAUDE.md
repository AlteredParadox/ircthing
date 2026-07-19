# CLAUDE.md — Project Instructions

## What this project is

A self-hosted, always-connected web IRC client shipped as a **single static Go binary**. The binary contains the bouncer core (persistent IRC connections, scrollback persistence) and serves the embedded web frontend. Think "The Lounge, but one small Go binary" — soju-class backend correctness with a first-class custom UI.

The design mockups in `design/` are the visual spec for the frontend. Match them — but `design/` is a **visual reference only**. It contains prototype HTML/JS exported from a design tool; its code, markup structure, and dependencies (React, Tailwind, etc.) must never be imported, copied, or ported. Extract the layout, spacing, typography, and color values, then reimplement from scratch within this project's frontend rules.

## Non-negotiable constraints

These are hard rules. If a task cannot be completed within them, stop and flag it rather than violating them.

1. **Single static binary.** `CGO_ENABLED=0`. Frontend assets embedded via `go:embed`. No runtime file dependencies except the config file and the SQLite database it creates.
2. **Binary size budget: 30 MB** (uncompressed, linux/amd64, `-ldflags="-s -w"`). Expected actual: 15–20 MB (modernc/sqlite accounts for most of it). If the binary approaches the gate, something is wrong — investigate, do not raise the budget.
3. **Frontend bundle budget: 100 KB gzipped**, measured as `gzip -9` of `dist/*.js` + `dist/*.css` (index.html excluded; if fonts or other assets are ever added, the gate must be extended to include them in the same commit). No exceptions for "just one component library."
4. **Memory target: under 72 MB RSS** with 5 networks / 50 channels / 10k messages of hot scrollback. Set `GOMEMLIMIT=64MiB` as the default in systemd units/docs. This is a documented target verified by a `make memcheck` scenario, not a hard CI gate (RSS is too noisy for pass/fail); regressions against it are bugs. Older scrollback lives in SQLite, not RAM.
5. **Dependency policy (Go):** stdlib first. Approved third-party deps:
   - `gopkg.in/irc.v4` (IRC message parsing)
   - `modernc.org/sqlite` (pure-Go SQLite — chosen deliberately: never mattn/go-sqlite3, which requires CGO and breaks the static-binary rule. Its ~1.5–2× query slowdown vs mattn is irrelevant at IRC message rates, and it is the dominant contributor to binary size — that is accepted. Do not swap drivers; if it ever becomes a problem, `ncruces/go-sqlite3` (WASM-based, still CGO-free) is the designated fallback, as a proposed change.)
   - `github.com/coder/websocket` (WebSocket — the maintained continuation of nhooyr.io/websocket; use only this import path)
   - `golang.org/x/crypto` (SCRAM, etc.)
   - `golang.org/x/term` (no-echo password read for `-hash-password`)
   - `golang.org/x/image` (pure-Go WebP decoder for the media-proxy thumbnailer — `internal/api/thumb.go`. Approved 2026-07-19: WebP is now the dominant image format served by CDNs/Google, and without it those image links render a blank card. CGO-free, +0.11 MB binary → 15.66 MB. Decode-only; we re-encode thumbnails to PNG/JPEG. Do NOT pull in x/image's other codecs (bmp/tiff) — WebP only, unless separately approved.)
   - `golang.zx2c4.com/wireguard` + `gvisor.dev/gvisor` (userspace WireGuard egress, `internal/wgdial` — the optional per-network `wireguard` proxy type). Approved after the phase-4 spike; the cost is recorded in `SPIKE.md` (+2.95 MB binary → 14.58 MB, well under the 30 MB gate; ~35 MB idle / ~58 MB peak RSS under `GOMEMLIMIT=64MiB` on the 1 vCPU / 1 GB target, zero DNS leaks). gVisor is a code-heavy but single module; do not pull in more of it than `tun/netstack` transitively needs.
   Anything else: propose it with a justification and wait for approval. No web frameworks — `net/http` + `ServeMux` is sufficient.
6. **Dependency policy (frontend):** Preact (or Solid — pick one at project start and stick to it), esbuild for bundling. No Tailwind, no component libraries, no icon packs (inline SVG only). Hand-written CSS with custom properties for theming.
7. **`make check` must pass before any task is considered done.** It runs: `go vet`, `staticcheck`, `go test ./...`, frontend build, binary size gate (30 MB), bundle size gate (100 KB gzipped). The binary size gate measures the **release build** (`-ldflags="-s -w"`); a separate `make build-debug` target produces an unstripped, `-race`-enabled binary for delve and is never size-gated. `make memcheck` (asserts on **RSS**, not GOMEMLIMIT, against the 72 MB target) is run before releases and after changes to buffering, caching, or the store.

## Architecture

```
cmd/ircd-web/          main: config, wiring, embedded assets
internal/irc/          per-network connection manager (one goroutine per network)
internal/store/        SQLite persistence: messages, networks, channels, read markers
internal/hub/          fan-out: IRC events -> connected WebSocket sessions
internal/api/          HTTP: auth, WebSocket endpoint, HTTP fallbacks (media proxy, search)
web/                   frontend source (built by esbuild, output embedded)
```

Concurrency model:
- One goroutine per IRC network connection (read loop) + one writer goroutine per connection with rate limiting.
- One goroutine per WebSocket client session.
- `internal/hub` owns fan-out; components communicate over channels. Shared mutable state is confined to the store and hub — no ad-hoc mutexes scattered through handlers.
- Reconnection with exponential backoff + jitter; replay missed history to clients from SQLite on reconnect.

Persistence:
- SQLite in WAL mode. Messages table indexed by (network, target, timestamp). FTS5 for server-side search.
- Hot scrollback cache in memory is bounded (ring buffer per target); everything else is queried from SQLite on demand.

Client sync protocol (browser <-> server WebSocket):
- JSON messages, versioned envelope. Server pushes IRC events; client requests history pages, sends messages, updates read markers. Multi-device read-marker sync is a core feature, not an afterthought.
- **Envelope forward-compatibility:** the Phase 1 protocol design must define message types for read markers and history paging from day one (even if Phase 1 stubs their server-side behavior), so Phase 2 extends the protocol rather than breaking it. Unknown message types must be ignored, not errored, on both ends.

## Protocol scope

### Core IRC (must be complete and correct)

- RFC 1459/2812 client protocol: registration, PING/PONG, JOIN/PART/QUIT/KICK/MODE/TOPIC/NICK, PRIVMSG/NOTICE, WHO/WHOIS/WHOWAS, LIST, MOTD, ISUPPORT (RPL_ISUPPORT parsing drives behavior: CASEMAPPING, PREFIX, CHANMODES, CHANTYPES, MODES, TARGMAX, LINELEN)
- TLS (including client certificates for SASL EXTERNAL), plaintext only with explicit config opt-in
- CTCP: ACTION, VERSION, PING, TIME, CLIENTINFO (DCC is explicitly out of scope)
- Correct casemapping per ISUPPORT (rfc1459, ascii) for all name comparisons
- Server password, WEBIRC not needed (we are the client), per-network egress proxy: SOCKS5/HTTP (`proxy`) or an in-process userspace WireGuard tunnel (`wireguard`; `internal/wgdial`, no TUN/root). WireGuard's Noise handshake avoids the cleartext SOCKS5/HTTP proxy-auth exposure; target DNS resolves through the tunnel (only the peer endpoint hostname resolves locally, pre-tunnel).
- Flood protection on send (token bucket per connection)

### IRCv3 — ratified (implement all)

- `CAP` negotiation (302), `cap-notify`
- `sasl` (3.2): PLAIN, EXTERNAL, SCRAM-SHA-256
- `message-tags`, `msgid`, `server-time`, `account-tag`
- `batch`, `labeled-response`, `echo-message`
- `account-notify`, `away-notify`, `chghost`, `extended-join`, `invite-notify`, `multi-prefix`, `userhost-in-names`, `setname`, `standard-replies`
- `monitor` (and `extended-monitor`)
- `STS` (strict transport security policy persistence)
- `UTF8ONLY`, `bot` mode awareness
- `WHOX` (not IRCv3 per se, but required for account discovery on join)

### IRCv3 — drafts we treat as required (modern client table stakes)

- `draft/chathistory` (+ `event-playback`) — both as *client of* servers/bouncers that offer it, and as the basis of our own scrollback replay to browsers
- `draft/read-marker` — bridge to our multi-device read sync
- `draft/typing` (client-only tag, send + render)
- `draft/multiline`
- `draft/message-redaction` (render deletions)
- `draft/no-implicit-names` (optimization)

When implementing any of these, fetch and follow the current spec at ircv3.net/specs — do not implement from memory. Note in a code comment which spec version/date was implemented.

### Explicitly out of scope

DCC file transfer/chat, IRCv3 vendor extensions other than soju.im ones we choose deliberately, server/IRCd functionality, federation, plugin system (for now).

## Frontend requirements

- Match the mockups in `design/`. Where mockups are silent, prefer density + keyboard efficiency.
- Virtualized message list — this is the one component where performance care is mandatory. Must stay smooth at 50k+ messages with variable heights.
- Consecutive-message grouping, nick colorization (deterministic hash), inline link previews (fetched via server-side proxy endpoint — never from the browser directly), image thumbnails via the same proxy with size caps.
- Responsive: usable at 360 px wide; sidebar collapses; touch targets sized appropriately.
- Keyboard-first: channel switcher palette (ctrl+k), unread navigation, tab-complete for nicks/commands/emoji.
- Theming via CSS custom properties; ship dark + light; user CSS override supported.
- Notifications: Web Notifications API + favicon badges; highlight rules configurable per network.
- No SPA router library — it is one screen with panels; hash-based state is fine.

## Testing

- Unit tests alongside all protocol code. Table-driven tests for message parsing/serialization edge cases.
- Integration: `make integration` spins up a local Ergo IRCd in a container and runs end-to-end tests (connect, SASL, join, chathistory, reconnect-replay).
- `make irctest` runs the client suite of `irctest` (https://github.com/progval/irctest) against the real binary: a pinned checkout plays the IRC server and drives our CAP/SASL/TLS/STS handshake via the controller in `integration/irctest/`. Needs `python3-venv`. Run it after changes to registration, SASL, TLS, or STS.
- Frontend: component tests for the virtualized list and input handling; no heavyweight e2e framework — a small Playwright suite for smoke tests only.

## Working rules for Claude Code

1. Run `make check` before declaring any task complete. If a size gate fails, fix the size — do not raise the budget.
2. Never add a dependency without asking. "It would be faster with library X" is a proposal, not a decision.
3. When implementing a protocol feature, fetch the spec (RFC or ircv3.net) and implement against it. Cite the spec section in comments for anything subtle (casemapping, CAP negotiation ordering, batch nesting, prefix parsing).
4. Prefer boring code. No reflection tricks, no code generation unless asked, no premature abstraction. This codebase should be readable by one tired sysadmin at 2 a.m.
5. Preserve the concurrency model. New cross-component communication goes through the hub; do not add shared state with mutexes to handlers.
6. Schema changes require a migration in `internal/store/migrations/` — never mutate schema in place.
7. Commits: small, single-purpose, imperative subject lines. Reference the spec/feature being implemented.

## Phasing

- **Phase 1 — usable core:** connect/TLS/SASL PLAIN, join/part/msg, SQLite persistence, WebSocket sync, basic UI per mockups, reconnect with replay. Ship criterion: daily-drivable on one network.
- **Phase 2 — modern client:** full ratified IRCv3 set, chathistory, read markers (multi-device), typing, previews/media proxy, search (FTS5), notifications.
- **Phase 3 — polish:** remaining drafts (multiline, redaction), SCRAM + EXTERNAL, monitor UI, themes, mobile refinement, irctest coverage.

Do not start a later phase's features while the current phase has open correctness bugs.
