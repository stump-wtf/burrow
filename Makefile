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

.PHONY: all build check lint fmt vet test race tidy clean run install version

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

test:
	$(GO) test ./...

# -race needs CGO enabled.
race:
	CGO_ENABLED=1 $(GO) test -race ./...

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
