.PHONY: build installcli installer test test-integration lint templint db-sh clean install check-templ generate check-node-modules css-build

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
	cd ui && npm install

## check-templ: Verify templ is installed
check-templ:
	@command -v templ >/dev/null 2>&1 || { echo "templ not found. Run: make install" >&2; exit 1; }

## check-node-modules: Auto-install npm deps if missing
check-node-modules:
	@[ -d ui/node_modules ] || (cd ui && npm install)

## css-build: Build Tailwind CSS for production (minified)
css-build: check-node-modules
	cd ui && npm run css:build

VERSION := $(shell git rev-parse --short HEAD 2>/dev/null || echo "dev")

## fmt: Format all Go source files
fmt:
	go fmt ./...

## build: Build the panel binary (bin/stackrd)
build: check-templ check-node-modules
	templ generate
	cd ui && npm run css:build
	hamr gen static
	go build -ldflags "-X main.version=$(VERSION)" -o bin/stackrd ./cmd/stackrd
	$(MAKE) generate

## installcli: Install the stackr CLI into GOBIN
installcli:
	go install -ldflags "-X main.version=$(VERSION)" ./cmd/stackr

## installer: Build the installer binary (bin/stackr-install)
installer:
	go build -ldflags "-X main.version=$(VERSION)" -o bin/stackr-install ./cmd/stackr-install

## generate: Generate static pages
generate:
	$(ENV_LOAD) ./bin/stackrd --generate

## test: Run all tests
test: check-templ
	templ generate
	go test ./...

## test-integration: Unit tests plus the build-tagged ones against the local Docker daemon
test-integration: check-templ
	templ generate
	go test -tags integration -count=1 ./...

## db-sh: Open an interactive shell to the local dev database
db-sh:
	$(ENV_LOAD) ./scripts/db-shell.sh

## lint: Run linters
lint:
	golangci-lint run

## templint: Lint .templ files for common issues
templint:
	hamr lint templ

## clean: Remove build artifacts
clean:
	rm -rf bin/ coverage.out coverage.html
