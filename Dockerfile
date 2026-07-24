# ircthing — self-hosted web IRC client, as a single static binary.
#
# Three stages so the runtime image carries only the binary + CA roots:
#   frontend  — esbuild the embedded web assets (web/dist is gitignored, so
#               it must be built here, not copied from the context)
#   builder   — compile the CGO-free static binary with the assets embedded
#   final     — minimal Alpine runtime, non-root, HTTP only (TLS at the proxy)
#
# Build (stamps the version the same way `make build` does):
#   docker build -t ircthing --build-arg VERSION=$(git describe --tags --always --dirty) .
# Or just `make docker`.

# ---- frontend: build web/dist with esbuild ----
# Pinned to the build platform: esbuild's output is architecture-independent
# JS/CSS, so this stage runs once and is reused for every target arch.
FROM --platform=$BUILDPLATFORM node:22-alpine AS frontend
WORKDIR /src
# make keeps the esbuild invocation identical to a local `make frontend`,
# so the image can never drift from the committed build.
RUN apk add --no-cache make
# Install deps first for layer caching; the source copy below invalidates
# less often than a dependency change would.
COPY web/package.json web/package-lock.json ./web/
RUN cd web && npm ci --no-fund --no-audit
COPY Makefile ./
COPY web ./web
# node_modules is restored from the npm ci layer (excluded by .dockerignore);
# touch it so make treats it as up to date and does not re-run npm ci.
RUN touch web/node_modules && make frontend

# ---- builder: compile the static binary ----
# Runs on the build platform and cross-compiles to the target arch. modernc
# SQLite is pure Go and CGO is off, so cross-compilation is a trivial GOARCH
# switch — no QEMU emulation of the (slow) Go compile for arm64 builds.
FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS builder
WORKDIR /src
# Module download is its own cached layer keyed on go.mod/go.sum only.
COPY go.mod go.sum ./
RUN go mod download
# Copy only what the build compiles or embeds — explicit paths (rather than a
# recursive `COPY . .`) keep unrelated and potentially sensitive files out of
# the build. Embedded assets: LICENSE + THIRD_PARTY_LICENSES.md (notices.go),
# internal/store/migrations/*.sql, and web/dist (from the frontend stage).
COPY notices.go LICENSE THIRD_PARTY_LICENSES.md ./
COPY cmd/ ./cmd/
COPY internal/ ./internal/
COPY web/embed.go ./web/
# Bring in the built frontend so //go:embed all:dist has real assets to embed.
COPY --from=frontend /src/web/dist ./web/dist
# TARGETOS/TARGETARCH are set automatically by buildx (default to the host on
# a plain `docker build`). VERSION defaults to a placeholder; pass
# --build-arg VERSION=$(git describe ...) to stamp the About panel / /api/config.
# REVISION (the exact commit) is stamped into main.revision so /source can pin
# it — buildinfo has none here (-trimpath, no .git in the context). Pass it only
# for a CLEAN build; leaving it empty makes /source fall back to the repo root.
ARG TARGETOS TARGETARCH VERSION=docker REVISION=
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath \
      -ldflags="-s -w -X main.version=${VERSION} -X main.revision=${REVISION}" \
      -o /out/ircd-web ./cmd/ircd-web

# ---- final: minimal runtime ----
# Track a current, supported Alpine branch (3.21 reaches EOL 2026-11); the
# minor tag auto-picks up patch releases with CVE fixes.
FROM alpine:3.24
# ca-certificates: the binary makes outbound TLS to IRC servers, the media
# proxy, and the push services — without the roots those all fail.
# tzdata: correct local-time rendering. wget (busybox): the HEALTHCHECK.
RUN apk add --no-cache ca-certificates tzdata \
 && addgroup -g 10001 -S ircthing \
 && adduser -u 10001 -S -G ircthing -h /var/lib/ircthing ircthing \
 && mkdir -p /var/lib/ircthing \
 && chown ircthing:ircthing /var/lib/ircthing
COPY --from=builder /out/ircd-web /usr/local/bin/ircd-web
USER ircthing
WORKDIR /var/lib/ircthing
# SQLite database (+ -wal/-shm) lives here; the config is mounted read-only
# at /etc/ircthing/config.json (see deploy/docker/).
VOLUME ["/var/lib/ircthing"]
EXPOSE 8067
# /license is a tiny, always-200, unauthenticated endpoint — proves the HTTP
# server is serving without needing a session.
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD wget -q -O /dev/null http://127.0.0.1:8067/license || exit 1
ENTRYPOINT ["/usr/local/bin/ircd-web", "-config", "/etc/ircthing/config.json"]
