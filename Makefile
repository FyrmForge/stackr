.PHONY: admin build installcli installer release release-images release-installer test test-integration lint templint db-sh clean install generate check-node-modules css-build

# Force bash so the ENV_LOAD eval below works cross-shell (sh on Debian/Ubuntu
# is dash, which doesn't grok `eval "$(...)"` quoting consistently).
SHELL := bash
.SHELLFLAGS := -ec

HAMR_VERSION  := $(shell grep 'github.com/FyrmForge/hamr ' go.mod | awk '{print $$2}')

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
	cd ui && npm install
	@# The fake host's proxy binds these as your user; Docker would make them root's.
	mkdir -p data/proxy-admin data/proxy-caddy

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
build: check-node-modules
	go tool templ generate
	cd ui && npm run css:build
	cd ui && npm run ts:build
	hamr gen static
	go build -ldflags "-X main.version=$(VERSION)" -o bin/stackrd ./cmd/stackrd
	$(MAKE) generate

ADMIN_EMAIL    ?= admin@test.com
ADMIN_PASSWORD ?= $(shell sed -n 's/^UNIVERSAL_STACKR_PASSWORD=//p' .env 2>/dev/null)

## admin: Make the dev admin on an empty DB (no /register; the installer does this on a box)
admin:
	@$(ENV_LOAD) printf '%s\n' '$(ADMIN_PASSWORD)' | go run ./cmd/stackrd create-admin --email '$(ADMIN_EMAIL)'

## installcli: Install the stackr CLI into GOBIN
installcli:
	go install -ldflags "-X main.version=$(VERSION)" ./cmd/stackr

## installer: Build the installer binary (bin/stackr-install)
installer:
	go build -ldflags "-X main.version=$(VERSION)" -o bin/stackr-install ./cmd/stackr-install

## release: stackrd and stackr-observe images + stackr-install for linux/amd64, e.g. make release RELEASE=v0.1.0
## A rig build (-dev.N) has no registry digest, so its installer pulls by tag.
## The release workflow runs the two halves itself: release-images, push, then
## release-installer DIGEST=sha256:... with the digest the registry answered.
## Rig test builds are vX.Y.Z-dev.N after the latest tag (v0.6.0-dev.1): never
## a git tag, and a -dev build is never offered a self-upgrade.
## The image is tagged without the v (ghcr.io/fyrmforge/stackr:0.1.0), as
## the installer and the self-upgrade look it up.
release: release-images release-installer

release-images:
	@[[ "$(RELEASE)" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-dev\.[0-9]+)?$$ ]] || { echo "usage: make release RELEASE=vX.Y.Z or vX.Y.Z-dev.N" >&2; exit 1; }
	$(MAKE) build
	docker build --platform linux/amd64 --build-arg VERSION=$(RELEASE) -f cmd/stackrd/Dockerfile -t ghcr.io/fyrmforge/stackr:$(RELEASE:v%=%) .
	docker build --platform linux/amd64 -f cmd/stackr-observe/Dockerfile -t ghcr.io/fyrmforge/stackr-observe:$(RELEASE:v%=%) .

## release-installer: the installer of RELEASE. A release (no -dev) needs
## DIGEST, the panel image's registry digest, embedded so the installer pulls
## that and never the tag.
release-installer:
	@[[ "$(RELEASE)" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-dev\.[0-9]+)?$$ ]] || { echo "usage: make release RELEASE=vX.Y.Z or vX.Y.Z-dev.N" >&2; exit 1; }
	@[[ "$(RELEASE)" == *-dev.* || "$(DIGEST)" =~ ^sha256:[0-9a-f]{64}$$ ]] || { echo "a release installer needs DIGEST=sha256:<64 hex>, the pushed panel image's digest" >&2; exit 1; }
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -ldflags "-X main.version=$(RELEASE) -X main.digest=$(DIGEST)" -o bin/stackr-install-linux-amd64 ./cmd/stackr-install

## generate: Generate static pages
generate:
	$(ENV_LOAD) ./bin/stackrd --generate

## test: Run all tests
test:
	go tool templ generate
	go test -timeout 20m ./...

## test-integration: Unit tests plus the build-tagged ones against the local Docker daemon
test-integration:
	go tool templ generate
	go test -tags integration -count=1 ./...

## db-sh: Open an interactive shell to the local dev database
db-sh:
	$(ENV_LOAD) ./scripts/db-shell.sh

## lint: Run linters
lint: check-node-modules
	golangci-lint run
	cd ui && npm run ts:check
	go run ./cmd/stackrd --dump-openapi | diff -u docs/openapi.json - || (echo "docs/openapi.json is stale: go run ./cmd/stackrd --dump-openapi > docs/openapi.json" && exit 1)

## templint: Lint .templ files for common issues
templint:
	hamr lint templ

## clean: Remove build artifacts
clean:
	rm -rf bin/ coverage.out coverage.html
