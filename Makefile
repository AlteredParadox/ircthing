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
	cp web/index.html web/dist/index.html

web/node_modules: web/package.json web/package-lock.json
	cd web && npm ci --no-fund --no-audit
	touch web/node_modules

check: vet staticcheck test build binary-size-gate bundle-size-gate
	@echo "check: OK"

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

# Spins up a local Ergo IRCd in a container and runs end-to-end tests.
integration:
	@echo "integration: not implemented yet (stub)"; exit 1

# RSS scenario: 5 networks / 50 channels / 10k hot messages under
# GOMEMLIMIT=64MiB, verified against the 72 MB target.
memcheck:
	@echo "memcheck: not implemented yet (stub)"; exit 1

clean:
	rm -rf bin
	find web/dist -type f ! -name .gitkeep -delete
