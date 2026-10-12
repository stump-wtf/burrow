# burrow — local dev Makefile.
#
# Mirrors .gitea/workflows/ci.yaml so what passes locally passes in CI.
# Common flows:
#
#   make              # build + check (the "did I break it" target)
#   make check        # just the CI gates (fmt, vet, test, race)
#
# Override the binary path / version via:
#   make VERSION=v0.1.0
#   make BIN_DIR=/usr/local/bin install

GO        ?= go
VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
BIN_DIR   ?= $(HOME)/.local/bin
PKG       := gitea.stump.rocks/stump.wtf/burrow
LDFLAGS   := -X $(PKG)/internal/buildinfo.Version=$(VERSION)
GOFLAGS   := -trimpath -ldflags "$(LDFLAGS)"
BIN       := burrow

.PHONY: all build check lint fmt vet test race test-db db-down tidy clean run install version

# The default "did I break it" loop.
all: build check

# Build the burrow binary into ./bin/ with version metadata baked in.
build:
	$(GO) build $(GOFLAGS) -o bin/$(BIN) ./cmd/burrow

# Full CI gate: formatting, vet, tests, and the race detector.
check: fmt vet test race

# Static checks only (the uniform `make lint` entry point).
lint: fmt vet

fmt:
	@unformatted=$$(gofmt -l $$(git ls-files '*.go' | grep -v '^vendor/')); \
	if [ -n "$$unformatted" ]; then \
		echo "gofmt would reformat:"; echo "$$unformatted"; exit 1; \
	fi

vet:
	$(GO) vet ./...

# -count=1 keeps Go's test cache out of the gate. The cache keys a result on
# the test binary, its flags and the environment it read — not on the Postgres
# server behind BURROW_TEST_DATABASE_URL — so an unchanged package reports
# `ok (cached)` whether or not a database is there to reach, and CI restores
# that cache between runs.
test: test-db
	$(GO) test -count=1 ./...

# -race needs CGO enabled.
race: test-db
	CGO_ENABLED=1 $(GO) test -race -count=1 ./...

# The store is Postgres (ADR-0002), so the suites need a real server. Locally
# this starts a disposable container and points the tests at it; CI exports
# BURROW_TEST_DATABASE_URL for its own sidecar and the target does nothing.
PG_IMAGE ?= postgres:17-alpine
PG_NAME  ?= burrow-test-pg
PG_PORT  ?= 55432
BURROW_TEST_DATABASE_URL ?= postgres://burrow:burrow@127.0.0.1:$(PG_PORT)/burrow?sslmode=disable
export BURROW_TEST_DATABASE_URL

# `origin`, not `-n`: the value is always non-empty because of the default
# above, so the only way to tell "someone set it" from "make defaulted it" is
# where it came from. Anything but this file's own default counts — CI's
# environment, and `make test BURROW_TEST_DATABASE_URL=…` on the command line,
# which would otherwise fall through to the docker branch and start a server
# nobody asked for.
ifneq ($(origin BURROW_TEST_DATABASE_URL),file)
test-db:
	@echo "test-db: using BURROW_TEST_DATABASE_URL from the $(origin BURROW_TEST_DATABASE_URL)"
else
# Readiness is probed over TCP (-h 127.0.0.1), never the unix socket: on a cold
# start the postgres image runs a temporary init server that listens on the
# socket only, answers pg_isready there, and then shuts down before the real
# server starts. A socket probe goes ready, not ready, ready, so a loop that
# breaks on the first "ready" hands the tests a server that is on its way down.
# The loop's own exit is the verdict — there is no second probe to land in the
# restart gap.
test-db:
	@if docker inspect -f '{{.State.Running}}' $(PG_NAME) 2>/dev/null | grep -q true; then \
		echo "test-db: reusing the running $(PG_NAME) container on :$(PG_PORT)"; \
	elif ! command -v docker >/dev/null 2>&1; then \
		echo "test-db: no docker here — export BURROW_TEST_DATABASE_URL pointing at a Postgres whose user may CREATE SCHEMA" >&2; \
		exit 1; \
	elif docker container inspect $(PG_NAME) >/dev/null 2>&1; then \
		echo "test-db: starting the stopped $(PG_NAME) container"; \
		docker start $(PG_NAME) >/dev/null || exit 1; \
	else \
		echo "test-db: starting $(PG_NAME) on :$(PG_PORT)"; \
		docker run -d --name $(PG_NAME) -p $(PG_PORT):5432 \
			-e POSTGRES_USER=burrow -e POSTGRES_PASSWORD=burrow -e POSTGRES_DB=burrow \
			$(PG_IMAGE) >/dev/null || exit 1; \
	fi; \
	for _ in $$(seq 1 60); do \
		docker exec $(PG_NAME) pg_isready -q -h 127.0.0.1 -U burrow >/dev/null 2>&1 && exit 0; \
		sleep 1; \
	done; \
	echo "test-db: $(PG_NAME) did not accept TCP connections within 60s — see: docker logs $(PG_NAME)" >&2; \
	exit 1
endif

# Stop and remove the disposable test server.
db-down:
	@docker rm -f $(PG_NAME) >/dev/null 2>&1 && echo "test-db: removed $(PG_NAME)" || true

# Apply formatting in place. Uses gofumpt if installed, falls back to gofmt.
tidy:
	@command -v gofumpt >/dev/null 2>&1 && gofumpt -w . || gofmt -w .
	$(GO) mod tidy

# Build and run burrow against its default address.
run: build
	./bin/$(BIN)

# Install the freshly-built binary into BIN_DIR (default ~/.local/bin).
install: build
	@mkdir -p $(BIN_DIR)
	install -m 0755 bin/$(BIN) $(BIN_DIR)/$(BIN)
	@echo "installed → $(BIN_DIR)/$(BIN)"

version:
	@echo $(VERSION)

clean:
	rm -rf bin/ dist/
