# ircthing — single static Go binary web IRC client.
# `make check` must pass before any task is considered done (see CLAUDE.md).

GO            ?= go
BIN           := bin/ircd-web
GOFLAGS       := -trimpath -ldflags="-s -w"
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
ESBUILD_FLAGS := --bundle --minify --format=esm \
	--jsx=automatic --jsx-import-source=preact \
	--target=es2020

.PHONY: build build-debug frontend check vet staticcheck test binary-size-gate bundle-size-gate integration memcheck clean

build: frontend
	CGO_ENABLED=0 $(GO) build $(GOFLAGS) -o $(BIN) ./cmd/ircd-web

# Unstripped, race-enabled binary for debugging with delve. Never
# size-gated; the release gate measures the stripped build above.
build-debug: frontend
	$(GO) build -race -o bin/ircd-web-debug ./cmd/ircd-web

frontend: web/node_modules
	cd web && $(ESBUILD) $(ESBUILD_FLAGS) src/main.jsx --outfile=dist/app.js
	cp web/index.html web/manifest.json web/icon.svg web/dist/

web/node_modules: web/package.json web/package-lock.json
	cd web && npm ci --no-fund --no-audit
	touch web/node_modules

check: vet staticcheck test frontend-test build binary-size-gate bundle-size-gate
	@echo "check: OK"

# Pure frontend logic (parsers, formatting) tested with node's built-in
# runner — no extra test dependencies.
frontend-test:
	cd web && node --test test/

vet:
	$(GO) vet ./...

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

# RSS scenario: 5 networks / 50 channels / 10k hot messages under
# GOMEMLIMIT=64MiB, verified against the 72 MB target.
memcheck:
	@echo "memcheck: not implemented yet (stub)"; exit 1

clean:
	rm -rf bin
	find web/dist -type f ! -name .gitkeep -delete
