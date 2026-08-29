# Single entry point for common bundle-docs workflows. Targets are
# grouped by what they touch so `make help` reads as a tour of the
# project. Variables are overridable on the command line, e.g.:
#
#   make refresh TAG=2026-05-21 REGISTRY=ghcr.io/xpqz
#   make db DOCS_REF=abcdef1234

# ---------- variables ---------------------------------------------------------

TAG           ?= latest
REGISTRY      ?= localhost
PLATFORMS     ?= linux/arm64
DOCS_REF      ?=
BUILD_VERSION ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo dev)
BUILD_TIME    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

GO_BUILD_TAGS    ?= fts5 semantic
GO_LDFLAGS       := -s -w -X 'main.buildVersion=$(BUILD_VERSION)' -X 'main.buildTime=$(BUILD_TIME)'
DB_PATH          := deploy/dyalog-docs.db

VENV     := .venv
VENV_PY  := $(VENV)/bin/python
VENV_PIP := $(VENV)/bin/pip

# ---------- meta --------------------------------------------------------------

.DEFAULT_GOAL := help

.PHONY: help
help:  ## list targets
	@awk 'BEGIN{FS=":.*## "}/^[a-zA-Z0-9_.-]+:.*## /{printf "  %-18s %s\n",$$1,$$2}' $(MAKEFILE_LIST)

# ---------- local build -------------------------------------------------------

.PHONY: build
build:  ## go build bundle-docs + docsearch (fts5+semantic) into ./bin
	@mkdir -p bin
	go build -tags "$(GO_BUILD_TAGS)" -ldflags "$(GO_LDFLAGS)" -o bin/bundle-docs .
	go build -tags "$(GO_BUILD_TAGS)" -ldflags "$(GO_LDFLAGS)" -o bin/docsearch ./cmd/docsearch

.PHONY: install
install:  ## go install into $GOPATH/bin
	go install -tags "$(GO_BUILD_TAGS)" -ldflags "$(GO_LDFLAGS)" .
	go install -tags "$(GO_BUILD_TAGS)" -ldflags "$(GO_LDFLAGS)" ./cmd/docsearch

# ---------- tests -------------------------------------------------------------

.PHONY: test test-go test-py
test: test-go test-py  ## run go (all three tag combos) + python tests

test-go:  ## go test with every supported tag combo
	go test -count=1 ./...
	go test -count=1 -tags fts5 ./...
	go test -count=1 -tags "$(GO_BUILD_TAGS)" ./...

test-py:  ## python tests (requires .venv)
	@test -x $(VENV_PY) || { echo "make test-py: $(VENV_PY) missing; run 'make venv' first" >&2; exit 1; }
	$(VENV_PY) -m unittest scripts/test_embedding_server.py -v

# ---------- python venv (only needed for the host-toolchain DB build) ---------

.PHONY: venv
venv: $(VENV_PY)  ## create .venv and install embedder deps

$(VENV_PY):
	python3 -m venv $(VENV)
	$(VENV_PIP) install --upgrade pip
	$(VENV_PIP) install -r scripts/requirements-embedding-server.txt

# ---------- artefacts ---------------------------------------------------------

.PHONY: db db-host
db:  ## build deploy/dyalog-docs.db reproducibly via Docker (default)
	PLATFORMS="$(PLATFORMS)" DOCS_REF="$(DOCS_REF)" \
	    BUILD_VERSION="$(BUILD_VERSION)" BUILD_TIME="$(BUILD_TIME)" \
	    deploy/build-db-docker.sh

db-host: venv build  ## same DB but using the local Go + Python toolchain (faster iteration)
	DOCS_REF="$(DOCS_REF)" deploy/build-db.sh

.PHONY: images push
images: $(DB_PATH)  ## build docsearch-web + docsearch-embedder images
	PLATFORMS="$(PLATFORMS)" TAG="$(TAG)" REGISTRY="$(REGISTRY)" deploy/build-images.sh

push:  ## build and push images to $REGISTRY:$TAG
	PUSH=1 PLATFORMS="$(PLATFORMS)" TAG="$(TAG)" REGISTRY="$(REGISTRY)" deploy/build-images.sh

# Convenience guard so `make images` errors clearly when the DB is missing.
$(DB_PATH):
	@echo "Missing $(DB_PATH); run 'make db' first." >&2
	@exit 1

# ---------- compose lifecycle -------------------------------------------------

.PHONY: up down logs restart status refresh
up:  ## docker compose up -d
	cd deploy && REGISTRY="$(REGISTRY)" TAG="$(TAG)" docker compose up -d

down:  ## docker compose down
	cd deploy && docker compose down

logs:  ## follow logs
	cd deploy && docker compose logs -f

restart:  ## recreate all containers (use after rebuilding images)
	cd deploy && REGISTRY="$(REGISTRY)" TAG="$(TAG)" docker compose up -d --force-recreate

status:  ## ps + healthchecks
	cd deploy && docker compose ps

refresh: db images restart  ## full rebuild + redeploy (db -> images -> recreate)

.PHONY: verify
verify:  ## post-deploy smoke: probe /api/health, /api/search ⎕IO, /api/version
	@deploy/verify.sh

# ---------- housekeeping ------------------------------------------------------

.PHONY: clean fmt vet
clean:  ## remove local build artefacts (keeps .venv)
	rm -rf bin $(DB_PATH)

fmt:  ## gofmt
	gofmt -w .

vet:  ## go vet all build configurations
	go vet ./...
	go vet -tags fts5 ./...
	go vet -tags "$(GO_BUILD_TAGS)" ./...

.PHONY: version
version: build  ## print embedded version info from a freshly-built docsearch
	./bin/docsearch version
