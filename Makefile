# ircthing — single static Go binary web IRC client.
# `make check` must pass before any task is considered done (see CLAUDE.md).

GO            ?= go
BIN           := bin/ircd-web
# VERSION stamps the binary (settings About + /api/config): the nearest
# tag with distance/hash when past it, and -dirty on an unclean tree.
# Falls back to the VCS buildinfo revision (main.effectiveVersion) when
# built without make.
VERSION       := $(shell git describe --tags --always --dirty 2>/dev/null)
GOFLAGS       := -trimpath -ldflags="-s -w -X main.version=$(VERSION)"
# staticcheck is run via `go run` (pinned) so it needs no global install
# and stays out of go.mod. GOTOOLCHAIN pins its build to the same Go
# version the module resolves, or it refuses to analyze the module.
STATICCHECK   := GOTOOLCHAIN=$(shell $(GO) env GOVERSION) $(GO) run honnef.co/go/tools/cmd/staticcheck@v0.7.0

# Size gates. Budgets are hard rules from CLAUDE.md — fix the size,
# never raise these numbers.
# 30 MB
BINARY_BUDGET_BYTES := 31457280
# 100 KB gzipped (total JS+CSS)
BUNDLE_BUDGET_BYTES := 102400

ESBUILD := node_modules/.bin/esbuild
# es2022 for top-level await (the SW registration in main.jsx). Every
# browser this app targets (anything with the Push API / installable
# PWAs) is comfortably past ES2022.
ESBUILD_FLAGS := --bundle --minify --format=esm \
	--jsx=automatic --jsx-import-source=preact \
	--target=es2022

.PHONY: build build-debug frontend check vet gofmt-check staticcheck test frontend-test browser-test binary-size-gate bundle-size-gate go-version-gate notices notices-check integration irctest memcheck clean docker

# The go.mod toolchain directive is the minimum Go patch level a release may
# be built with (stdlib CVE fixes ship in patch releases; an older toolchain
# reintroduces them into the binary). GOTOOLCHAIN=auto honors the directive
# automatically — this gate catches GOTOOLCHAIN=local overrides and stale
# system toolchains so a vulnerable build fails loudly instead of shipping.
go-version-gate:
	@want=$$(awk '/^toolchain /{print $$2}' go.mod); \
	got=$$($(GO) env GOVERSION); \
	if [ -n "$$want" ] && [ "$$(printf '%s\n' "$$want" "$$got" | sort -V | head -1)" != "$$want" ]; then \
		echo "FAIL: building with $$got, but go.mod requires >= $$want"; \
		exit 1; \
	fi

# `notices` runs as a prerequisite: THIRD_PARTY_LICENSES.md is embedded into
# the binary (notices.go), so regenerating it here means what ships always
# matches what is actually linked in — there is no separate step to forget
# after a dependency bump. The committed copy exists so that a bare `go build`
# (and the Dockerfile, which COPYs it rather than regenerating) still works.
build: go-version-gate frontend notices
	CGO_ENABLED=0 $(GO) build $(GOFLAGS) -o $(BIN) ./cmd/ircd-web

# Unstripped, race-enabled binary for debugging with delve. Never
# size-gated; the release gate measures the stripped build above.
build-debug: frontend
	$(GO) build -race -o bin/ircd-web-debug ./cmd/ircd-web

# Build the container image (deploy/docker/). Stamps the same VERSION as the
# native build so the About panel matches. The frontend is built inside the
# image; this needs only Docker, not node/go on the host.
# DOCKER_REVISION pins /source to the exact commit — but only when the tree is
# FULLY clean, so a dirty local image never claims a commit that doesn't reflect
# it. `git describe --dirty` ignores untracked files (which the explicit COPYs
# in the Dockerfile would still pull in), so gate on `git status --porcelain
# --untracked-files=normal` being empty instead. Capture status into a var and
# require the command to SUCCEED before the emptiness test — otherwise a git
# error (empty stdout) would read as "clean" and wrongly stamp HEAD.
DOCKER_REVISION := $(shell s=$$(git status --porcelain --untracked-files=normal 2>/dev/null) && test -z "$$s" && git rev-parse HEAD 2>/dev/null)
docker:
	docker build -t ircthing:local \
	  --build-arg VERSION=$(VERSION) \
	  --build-arg REVISION=$(DOCKER_REVISION) .

frontend: web/node_modules
	cd web && $(ESBUILD) $(ESBUILD_FLAGS) src/main.jsx --outfile=dist/app.js
	# Service worker: separate entry, classic script (iife) — Safari's
	# module-SW support isn't worth depending on. Counted by the bundle gate.
	cd web && $(ESBUILD) --bundle --minify --format=iife --target=es2022 src/sw.js --outfile=dist/sw.js
	cp web/index.html web/manifest.json web/icon.svg web/dist/

# --ignore-scripts: dependency lifecycle scripts are arbitrary code run at
# install time, and nothing in this tree needs them (esbuild ships its
# platform binary as an optionalDependency; Playwright's browsers come from an
# explicit `playwright install`, not a postinstall hook). Matches CI.
web/node_modules: web/package.json web/package-lock.json
	cd web && npm ci --no-fund --no-audit --ignore-scripts
	touch web/node_modules

check: vet gofmt-check staticcheck test frontend-test build binary-size-gate bundle-size-gate
	@echo "check: OK"

# Regenerate the third-party notices from the modules actually linked in.
# Depends on `frontend`: the generator does a probe build (which embeds
# web/dist) and reads the bundled npm packages out of web/node_modules.
notices: frontend
	@./scripts/gen-third-party-licenses.sh >/dev/null

# Fail if the COMMITTED notices are stale. Deliberately NOT part of `check`:
# Dependabot bumps go.mod but cannot regenerate this file, so gating every PR
# on it makes each gomod PR red for a reason the bot can't fix — every bump
# then needs a hand-pushed commit onto its branch. `build` regenerates instead,
# so the artifact is always correct; this gate runs in the release workflow,
# which is the point where the notice legally has to match what is published
# (the Docker image COPYs this file rather than regenerating it).
#
# ORDERING: run this BEFORE anything that invokes `make build`. A build
# rewrites THIRD_PARTY_LICENSES.md in the working tree, after which this
# diff trivially passes and the gate is worthless.
#
# Depends on `frontend` for the same reason `notices` does — the generator
# probe-builds the binary and reads web/node_modules/preact. Both are absent
# on a fresh clone, so without this the target only works on a warm tree.
notices-check: frontend
	@tmp=$$(mktemp); \
	./scripts/gen-third-party-licenses.sh "$$tmp" >/dev/null; \
	if ! diff -q THIRD_PARTY_LICENSES.md "$$tmp" >/dev/null; then \
		echo "FAIL: THIRD_PARTY_LICENSES.md is stale — run 'make notices' and commit the result"; \
		diff -u THIRD_PARTY_LICENSES.md "$$tmp" | head -40; \
		rm -f "$$tmp"; exit 1; \
	fi; \
	rm -f "$$tmp"; \
	echo "notices-check: THIRD_PARTY_LICENSES.md is current"

# Pure frontend logic (parsers, formatting) tested with node's built-in
# runner — no extra test dependencies.
frontend-test:
	cd web && node --test

vet:
	$(GO) vet ./...

# Formatting gate: fail if any tracked .go file isn't gofmt-clean. Scoped
# to git-tracked files so the vendored checkouts under .cache/ (gitignored)
# are never scanned. Simplifications (gofmt -s) are left to staticcheck.
gofmt-check:
	@bad=$$(gofmt -l $$(git ls-files '*.go')); \
	if [ -n "$$bad" ]; then \
		echo "gofmt: these files need formatting (run: gofmt -w <file>):"; \
		echo "$$bad"; exit 1; \
	fi

staticcheck:
	$(STATICCHECK) ./...

test:
	$(GO) test ./...

binary-size-gate: build
	@size=$$(stat -c%s $(BIN)); \
	echo "binary size: $$size bytes (budget: $(BINARY_BUDGET_BYTES))"; \
	if [ "$$size" -gt "$(BINARY_BUDGET_BYTES)" ]; then \
		echo "FAIL: $(BIN) exceeds the 30 MB binary budget"; \
		exit 1; \
	fi

bundle-size-gate: frontend
	@total=0; \
	for f in web/dist/*.js web/dist/*.css; do \
		[ -f "$$f" ] || continue; \
		s=$$(gzip -9 -c "$$f" | wc -c); \
		echo "  $$f: $$s bytes gzipped"; \
		total=$$((total + s)); \
	done; \
	echo "bundle size: $$total bytes gzipped (budget: $(BUNDLE_BUDGET_BYTES))"; \
	if [ "$$total" -gt "$(BUNDLE_BUDGET_BYTES)" ]; then \
		echo "FAIL: JS+CSS bundle exceeds the 100 KB gzipped budget"; \
		exit 1; \
	fi

# End-to-end tests against a real Ergo IRCd (connect, SASL, join,
# chathistory, reconnect-replay). Ergo is a pure-Go binary, so it runs
# directly — no container runtime needed; ERGO_BIN overrides the cached
# build.
ERGO_REF := v2.19.0-rc1

integration: .cache/bin/ergo
	go test -tags integration -count=1 -v -timeout 300s ./integration/

ergo: .cache/bin/ergo

.cache/bin/ergo:
	@echo "building ergo ($(ERGO_REF)) into .cache/bin ..."
	rm -rf .cache/ergo-src
	git clone --depth 1 --branch $(ERGO_REF) https://github.com/ergochat/ergo.git .cache/ergo-src
	cd .cache/ergo-src && GOTOOLCHAIN=auto $(GO) build -o ../bin/ergo .

# irctest (https://github.com/progval/irctest) client-behavior tests:
# irctest plays the server and drives our CAP/SASL/TLS/STS handshake via
# the controller in integration/irctest/. Needs python3-venv installed.
IRCTEST_REF := a468d9fcd64abc72b02ecb20f4f8612fd72c8829

irctest: build .cache/irctest-src .cache/irctest-venv
	cd .cache/irctest-src && \
	IRCTHING_BIN=$(CURDIR)/bin/ircd-web \
	PYTHONPATH=$(CURDIR)/integration/irctest:$(CURDIR)/.cache/irctest-src \
	$(CURDIR)/.cache/irctest-venv/bin/pytest irctest/client_tests \
		--controller=ircthing_irctest -p ircthing_secure_sasl \
		-p no:cacheprovider --timeout=60

.cache/irctest-src:
	rm -rf .cache/irctest-src
	git init -q .cache/irctest-src
	cd .cache/irctest-src && \
	git remote add origin https://github.com/progval/irctest.git && \
	git fetch -q --depth 1 origin $(IRCTEST_REF) && \
	git checkout -q FETCH_HEAD

.cache/irctest-venv:
	python3 -m venv .cache/irctest-venv
	.cache/irctest-venv/bin/pip install --quiet pytest pytest-timeout filelock

# RSS scenario: 5 networks / 50 channels / 10k hot messages under
# GOMEMLIMIT=64MiB, asserted against the 72 MB RSS target. Run before
# releases and after changes to buffering, caching, or the store — not
# part of `make check` (RSS is too noisy for CI pass/fail).
# Browser layout tests (web/browser-test/): the real <Chat> in a real
# Chromium against the real stylesheet. Deliberately NOT part of `make check`
# — it needs a ~260 MB browser download, and `check` must stay runnable on a
# fresh clone. Run in CI as its own job, and locally after touching the
# virtualized list, row markup, or the stylesheet.
#
# Output goes to web/browser-test/.build, never web/dist: the bundle size gate
# globs web/dist/*.js and would count the harness against the 100 KB budget.
BROWSER_BUILD := web/browser-test/.build

browser-test: frontend
	@command -v node >/dev/null || { echo "FAIL: node required"; exit 1; }
	@cd web && node -e "require.resolve('playwright')" 2>/dev/null || { \
		echo "FAIL: playwright not installed — run: cd web && npm ci && npx playwright install --with-deps chromium"; \
		exit 1; \
	}
	mkdir -p $(BROWSER_BUILD)
	cd web && $(ESBUILD) --bundle --format=iife --target=es2022 \
		--jsx=automatic --jsx-import-source=preact \
		test/harness/chat-harness.jsx --outfile=browser-test/.build/chat-harness.js
	cp web/dist/app.css $(BROWSER_BUILD)/
	cp web/test/harness/chat-harness.html $(BROWSER_BUILD)/
	# Explicit glob: these are named *.browser.js precisely so `node --test`'s
	# default discovery (which sweeps **/*.test.js) leaves them to this target.
	cd web && node --test browser-test/*.browser.js

memcheck: build
	IRCTHING_BIN=$(CURDIR)/$(BIN) \
	go test -tags memcheck -count=1 -v -timeout 300s -run TestMemoryScenario ./integration/

clean:
	rm -rf bin
	find web/dist -type f ! -name .gitkeep -delete
