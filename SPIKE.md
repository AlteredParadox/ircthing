# SPIKE: in-process WireGuard egress

**Status: SHIPPED on `main` (2026-07-18).** This document is the historical spike
record — the cost investigation and the 1 vCPU / 1 GB target-box validation
(§6). The feature, its **web UI** (Egress → WireGuard tunnel), the approved
dependencies, and the pre-merge fixes are all merged; the shipped design is
documented in `CLAUDE.md` and `README.md`. Earlier "measurement only / do NOT
merge / no UI / deferred" notes below are preserved as they were written and are
superseded by this status.

Goal: measure the cost of routing a network's egress through an in-process
userspace WireGuard tunnel (wireguard-go + gVisor netstack — no TUN device, no
root, no host routing changes), as a new proxy type alongside `socks5`/`http`.
Motivation: SOCKS5/HTTP proxy auth is cleartext; WireGuard's Noise handshake is
not, and in-process tunneling leaves the web listener and other networks
untouched.

Bottom line: **it fits the binary budget with room to spare (14.6 MB, gate is
30 MB)**, idle cost is negligible, but gVisor's netstack buffer pools make RSS
under sustained traffic the one thing that must be re-validated on the 1-vCPU /
1 GB target before committing.

---

## 1. Dependency delta (go.sum / go.mod)

`go get golang.zx2c4.com/wireguard@latest` + importing its `tun/netstack`
package pulls **6 new modules**. go.sum grew by 12 lines (2 per module: the zip
hash and the go.mod hash). gVisor is a single module (code-heavy, but one entry).

| Module | Version | Direct? | Note |
|---|---|---|---|
| `golang.zx2c4.com/wireguard` | `v0.0.0-20260522210424-ecfc5a8d5446` | direct | the WireGuard implementation + `tun/netstack` |
| `gvisor.dev/gvisor` | `v0.0.0-20250503011706-39ed1f5ac29c` | indirect | userspace TCP/IP stack (the big one) |
| `golang.org/x/net` | `v0.56.0` | indirect | pulled by netstack |
| `github.com/google/btree` | `v1.1.2` | indirect | used by netstack |
| `golang.org/x/time` | `v0.7.0` (was a 2022 pseudo-version) | indirect | bumped |
| `golang.zx2c4.com/wintun` | `v0.0.0-20230126152724-0fa3db229ce2` | indirect | Windows TUN; in the graph, not linked on linux |

No new entries for `golang.org/x/crypto` or `golang.org/x/sys` — wireguard-go
uses versions already present via our existing `x/crypto` dependency.

> Note on the module path: `tun/netstack` also exists as a **legacy standalone
> module** (`golang.zx2c4.com/wireguard/tun/netstack`, last tagged 2022). Naively
> `go mod tidy`ing produces an "ambiguous import" because the package resolves
> from both the standalone module and the current main module. Pinning only the
> main `wireguard` module (which now contains `tun/netstack`) resolves it. If
> this is ever productionized, keep the standalone module out of go.mod.

Per CLAUDE.md this is a **dependency proposal**: `golang.zx2c4.com/wireguard`
and its transitive `gvisor.dev/gvisor` are not on the approved list. The gate
decision (approve the deps / raise nothing) belongs in review, recorded in the
commit that would change the policy.

---

## 2. Measurements

**Dev machine, NOT the target box.** 2× Xeon E5-2680 v4 @ 2.40 GHz (2 vCPU
visible), 7.7 GB RAM, linux/amd64, Go 1.25, `GOMAXPROCS=2`. The deployment
target is **1 vCPU / 1 GB** — every number below is dev-machine and must be
re-validated there. Throughput and CPU-per-MB in particular will be worse on 1
vCPU.

### 2a. Release binary size (`make check` gate: 30 MB)

Release build (`CGO_ENABLED=0`, `-trimpath -ldflags="-s -w"`, linux/amd64),
with the tunnel wired in and linked (dialer reachable from `main`, so gVisor is
NOT dead-code-eliminated):

| Build | Bytes | MB |
|---|---|---|
| baseline (`main`, no WireGuard) | 12,193,976 | 11.63 |
| spike (WireGuard + gVisor netstack) | **15,286,456** | **14.58** |
| **delta** | **+3,092,480** | **+2.95** |

**`make check` binary-size gate: PASS** — 14.58 MB against the 30 MB budget,
**15.4 MB of headroom**. gVisor's netstack (just the TCP/IP + `tun` packages)
links far leaner than the full gVisor sandbox platform would. **The 30 MB gate
was not touched.**

### 2b. RSS (one tunnel up, one connection)

Measured with a **real loopback WireGuard pair** (two userspace wireguard-go
devices, each on its own netstack, peering over 127.0.0.1 UDP; handshake
completes; traffic really traverses the tunnel). Harness:
`internal/wgdial/bench_wgbench_test.go` (build tag `wgbench`).

| State | VmRSS | Δ |
|---|---|---|
| baseline (Go runtime + harness, no tunnel) | 8.0 MB | — |
| **one** userspace WG device up (netstack, idle) | 10.7 MB | **+2.7 MB / device** |
| tunnel up + 1 TCP connection (two devices in-process) | 12.2 MB | per-client-tunnel incremental **~1.5 MB** |

**One client tunnel idle costs roughly +1.5 to +2.7 MB RSS.** Negligible
against the 72 MB target.

Cross-check via `runtime.MemStats` at the "connected" point: `HeapAlloc`
**49 MB** while VmRSS is only **12 MB**. That gap is real and important:
gVisor's netstack `make()`s large buffer pools that Go counts as heap but whose
pages are never faulted in until traffic touches them. So idle RSS is small even
though the reserved heap looks large.

### 2c. RSS under load — the one to watch

Pushing **50 MB** through the tunnel as fast as it will go (worst case, both
encrypt and decrypt sides running in the same process):

| Point | VmRSS | HeapAlloc | Sys (reserved) |
|---|---|---|---|
| peak right after the 50 MB flood | **101.6 MB** | 80.1 MB | 168.2 MB |
| after `debug.FreeOSMemory()` (retained) | **26.3 MB** | 73.0 MB | — |

The flood faults in the netstack buffer pools → RSS spikes to ~100 MB, then
Go returns the cold pages and it settles to ~26 MB. Caveats that pull the real
number **down**:

- This process runs **both** tunnel endpoints; a real deployment runs only the
  client side (roughly half the buffer churn).
- The bench does **not** set `GOMEMLIMIT`. Production ships `GOMEMLIMIT=64MiB`,
  which forces GC to hold RSS near the limit — trading the RSS spike for CPU.
- IRC / chathistory replay is **not** a 50 MB sustained flood; it's small bursty
  batches (KB to low MB). This is a synthetic stress ceiling, not the workload.

Still: a userspace netstack that can transiently reserve ~100 MB under load is
exactly the risk the 72 MB / 1 GB budget cares about. **Re-run 2b/2c on the
1-vCPU box with `GOMEMLIMIT=64MiB`, client-side only, before any merge decision.**

### 2d. CPU

- **Idle keepalive:** 4.7 ms of CPU over 120 s for **both** devices (25 s
  persistent-keepalive) = 2.3 ms/min total, ~1.2 ms/min per tunnel.
  Effectively free.
- **Replay throughput / cost:** 50 MB in 1.67 s = **30 MB/s**, **46.85 ms
  CPU/MB** across both encrypt+decrypt sides (~23 ms/MB one-directional). A
  large 5 MB chathistory replay ≈ 0.17 s wall and ~0.12 s one-side CPU on this
  box — fine for IRC. On 1 vCPU expect proportionally less throughput / more
  wall time; still comfortably above IRC data rates.

---

## 3. What was built

Minimal, self-contained, behind a config flag; no UI, no key management.

- **`internal/wgdial/`** — the dialer. `New(Config)` brings up a userspace
  WireGuard device on a gVisor netstack (no TUN, no root); `DialContext(ctx,
  addr)` dials a TCP target **through** the tunnel; `Close()` tears it down.
  `Validate(Config)` does side-effect-free static checks (keys decode to 32
  bytes, address/DNS parse, endpoint is host:port). Config = private key, peer
  public key, optional preshared key, endpoint, in-tunnel address, in-tunnel DNS.
- **DNS leak rule (matches the SOCKS5 dialer):** target hostnames are handed to
  `netstack.Net.DialContext`, which resolves them via the tunnel's configured
  in-band DNS server — never the local resolver.
- **`internal/irc`** — `Config.WireGuard *wgdial.Config`; mutually exclusive
  with `Proxy` (validated). `manager.dial` gains a WireGuard branch that builds
  the tunnel **lazily on first dial** (not in `NewManager`, so config validation
  on a throwaway manager has no side effects) and reuses it across reconnects; a
  build failure leaves it nil so backoff retries. Torn down when `Run` returns.
- **`internal/netconf`** — a `wireguard` object on a network (config-file only);
  its presence is the per-network flag. Mapped into `irc.Config` by `IRCConfig()`.

Example network config (config-file only, no UI):

```json
{
  "name": "libera-wg",
  "addr": "irc.libera.chat:6697",
  "tls": true,
  "nick": "...",
  "wireguard": {
    "private_key": "<base64>",
    "peer_public_key": "<base64>",
    "endpoint": "203.0.113.x:51820",
    "address": "10.64.0.2",
    "dns": "10.64.0.1"
  }
}
```

### Reproduce the measurements

```sh
# binary size (with the feature linked):
make build && stat -c%s bin/ircd-web

# RSS + CPU (real loopback tunnel; ~2 min with the default idle window):
WGBENCH_IDLE_SEC=120 WGBENCH_REPLAY_MB=50 \
  go test -tags wgbench -run TestWGBench -v -timeout 900s ./internal/wgdial/
```

---

## 4. Explicitly not done (per the handoff — stop here)

No Mullvad/provider key management, no config UI, no user docs. Also deferred as
out of spike scope: production tunnel-rebuild/backoff coordination, MTU tuning,
IPv6-endpoint edge cases, and end-to-end DNS-leak verification against a real
in-tunnel resolver (the loopback bench dials by IP, so it exercises the tunnel
data path but not the netstack resolver path — that path is implemented and is
how wireguard-go's netstack is designed to resolve, but it is not yet asserted
against a live DNS server).

## 5. Recommendation

The binary cost (+2.95 MB, 14.6 MB total) and idle cost (~2 MB RSS, ~1 ms/min
CPU per tunnel) are clearly acceptable. The open question is RSS under load: the
netstack buffer pools can transiently reserve ~100 MB in a both-sides-in-process
worst case. Before a merge decision, re-run 2b–2d on the **1 vCPU / 1 GB** box,
**client-side only**, **with `GOMEMLIMIT=64MiB`**, using an IRC-shaped workload
(a real chathistory replay, not a 50 MB flood). If RSS there stays within the
72 MB target under GOMEMLIMIT pressure without unacceptable GC CPU, this is a
viable phase-4 feature and the dependency (`gvisor.dev/gvisor`) can be proposed
for approval with these numbers on record.

---

## 6. Target-box validation (host-a, 2026-07-18)

**Environment:** 1 vCPU / 1 GB VPS, Debian 13 (6.12 cloud kernel),
`GOMEMLIMIT=64MiB`, `GODEBUG=gctrace=1`, hardened DynamicUser unit. Real WG
peer on a fleet host (host-b, port 51821 — a resident WireGuard service owns 51820), tunnel
DNS = 9.9.9.9 resolved in-tunnel, client-side only (unlike §2's
both-sides-in-process bench). Network: libera/#linux via hostname, TLS.

| metric | result | budget/req |
|---|---|---|
| DNS leak — tcpdump on real ifaces during connect+join, live port-53 workload | 0 packets escaped | 0 |
| DNS leak — failure paths (multiple dial timeouts/retries observed 00:42–01:01) | no local-resolver fallback | 0 |
| idle RSS after 6.7 h connected | 35.1 MB (VmHWM = VmRSS: never exceeded) | <72 MB |
| GC at idle | forced 2-min cadence, ~1.5 ms/cycle, 0% CPU; heap 25 MB vs 50 MB goal | no thrash |
| replay (full overnight #linux scrollback, 1 client) — peak CPU | 9% for ~2 s, 0.65% avg | usable on 1 vCPU |
| replay — peak heap / peak RSS | 51 MB / 57.9 MB | <72 MB (transient) |
| replay — retained heap after GC | 29→27 MB (baseline 25); worst GC pause 7.6 ms | no retention |
| restarts / crashes over soak | 0 | — |

RSS caveat: post-replay RSS holds at ~58 MB due to MADV_FREE lazy reclaim;
live heap confirms ~zero retention. Effective settled cost ≈ idle + noise.

### Findings (pre-merge fixes)

1. **Endpoint must accept hostnames.** UAPI takes literal IPs only
   (`ParseAddr` error). Resolve via the LOCAL resolver pre-tunnel — the one
   intentional exception to the leak rule (chicken-and-egg); comment it as
   such. Re-resolve on reconnect.
2. **v6-less kernels hard-fail the bind** (`EAFNOSUPPORT` bringing up the
   device). Budget-VPS target hardware; degrade to v4-only when endpoint is
   v4. (Surfaced initially via the sandbox, but applies to genuinely
   v6-disabled kernels too.)
3. **Unit needs `AF_NETLINK`** in `RestrictAddressFamilies` for the WG bind
   path. Update deploy/ unit with a do-not-remove comment; failure mode
   (EAFNOSUPPORT) only manifests on WG-enabled networks.
4. **`dns` field should accept host:port** (default :53). Port 53 isn't
   always usable at the far end (a resident WireGuard service DNS handling, port squatters).
5. **Replay churn:** ~25 MB transient alloc per replay stream, zero
   retained. Consider pooling encode/send buffers to flatten the transient;
   low priority, no leak.

### Rig notes (for future spikes)

- a resident WireGuard service hosts: 51820/udp is taken; a resident WireGuard service's DNS machinery affects port 53
  handling — run any test resolver off-port, or skip resolver infra entirely.
- The leak oracle is client-side tcpdump. Don't build peer-side resolver
  infrastructure to prove a client-side property (a public resolver dialed
  through the tunnel tests the same thing, harder).
- Fleet drop-policy forward chains: accepts must be inserted INTO the fleet
  chain — accept in a parallel chain doesn't exempt packets from other
  hooks' drops. Manual rules die on the next Ansible push (by design).
- DynamicUser state lives at /var/lib/private/<name>; wiping the
  /var/lib/<name> symlink doesn't wipe state.
- `fwd` is a reserved nft keyword; don't name chains that.

### Verdict

**Merge approved**, contingent on findings 1–4 (5 optional). Approve
`golang.zx2c4.com/wireguard` and `gvisor.dev/gvisor` onto the CLAUDE.md
allowlist citing this document. Binary 14.58 MB vs 30 MB gate — no budget
changes.
