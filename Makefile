.PHONY: deploy-test build test e2e lint templint openapi db-refresh clean install check-templ generate check-node-modules css-build proxyrelay installcli

# Force bash so the ENV_LOAD eval below works cross-shell (sh on Debian/Ubuntu
# is dash, which doesn't grok `eval "$(...)"` quoting consistently).
SHELL := bash
.SHELLFLAGS := -ec

HAMR_VERSION  := $(shell grep 'github.com/FyrmForge/hamr ' go.mod | awk '{print $$2}')
TEMPL_VERSION := $(shell grep 'github.com/a-h/templ ' go.mod | awk '{print $$2}')

# ENV_LOAD: prefix targets that need hamr dev's port-walked .env values
# (DATABASE_URL, S3_ENDPOINT, etc.). `hamr env --export` reads
# .hamr/walks.json (written by `hamr dev` after walking) + .env, and emits
# `export KEY=VALUE` lines for the values that were rewritten. When hamr
# dev isn't running or no ports walked, output is empty and eval is a
# no-op — the target falls through to whatever .env it already had.
# `|| true` survives the case where `hamr` isn't on PATH (e.g. during
# `make install`); plain .env still loads normally inside the binary.
ENV_LOAD := eval "$$(hamr env --export 2>/dev/null || true)";

## install: Install development dependencies
install:
	go install github.com/FyrmForge/hamr/cmd/hamr@$(HAMR_VERSION)
	go install github.com/a-h/templ/cmd/templ@$(TEMPL_VERSION)
	cd frontend && npm ci

## check-templ: Verify templ is installed
check-templ:
	@command -v templ >/dev/null 2>&1 || { echo "templ not found. Run: make install" >&2; exit 1; }

## check-node-modules: Auto-install npm deps if missing
check-node-modules:
	@[ -d frontend/node_modules ] || (cd frontend && npm install)

## css-build: Build Tailwind CSS for production (minified)
css-build: check-node-modules
	cd frontend && npm run css:build

VERSION := $(shell git rev-parse --short HEAD 2>/dev/null || echo "dev")

## fmt: Format all Go source files
fmt:
	go fmt ./...

## build: Build the stackrd binary
build: check-templ check-node-modules
	templ generate
	cd frontend && npm run css:build
	hamr gen static
	go build -ldflags "-X main.version=$(VERSION)" -o bin/stackrd ./cmd/stackrd
	$(MAKE) generate

## installcli: Build and install the stackr CLI to GOBIN (~/go/bin by default)
installcli:
	go install -ldflags "-X github.com/FyrmForge/stackr/internal/cli/cmd.version=$(VERSION)" ./cmd/stackr

## proxyrelay: Build the proxyrelay binary and its image (stkr-proxyrelay:local)
# stackr creates one of these per active `stackr forward` target; it looks the
# image up by name and errors if missing, so run this once per dev machine.
proxyrelay:
	CGO_ENABLED=0 go build -ldflags "-s -w" -o bin/proxyrelay ./cmd/proxyrelay
	docker build -q -f cmd/proxyrelay/Dockerfile.runtime -t stkr-proxyrelay:local bin/

## generate: Generate static pages
generate:
	$(ENV_LOAD) ./bin/stackrd --generate

## test: Run all tests
test: check-templ
	templ generate
	go test ./...

## e2e: Browser tests (Chromium, headed). E2E_HEADLESS=1 for CI. Not in `make test`.
e2e: check-templ
	templ generate
	go test -tags e2e -count=1 -run TestE2E ./internal/stackrd/handlers/web/

## db-refresh: Delete the dev database; migrations recreate it on next server start
db-refresh:
	rm -f data/stackr.db data/stackr.db-wal data/stackr.db-shm data/stackr.db-journal

## openapi: Regenerate docs/openapi.json from the code-first spec
openapi:
	go run ./cmd/stackrd --dump-openapi

## lint: Run linters
lint:
	golangci-lint run

## templint: Lint .templ files for common issues
# Run from internal/ — every real .templ lives there, and the repo root's
# data/ holds dev-server build clones the linter must not sweep.
templint:
	cd internal && hamr lint templ --config ../hamr.toml

## clean: Remove build artifacts
clean:
	rm -rf bin/ coverage.out coverage.html

## deploy-test: Build and deploy to a disposable test VM
deploy-test:
	./scripts/dev/deploy-test.sh
