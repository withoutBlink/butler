# Butler — build and development tasks.
#
#   make             build both binaries (default)
#   make butler      the server only
#   make butler-mcp  the MCP server only
#   make check       go vet + go test
#   make install     go install both commands into GOBIN
#
# Overridables: BIN_DIR (default: the repository root, where the quickstart and
# .gitignore expect ./butler and ./butler-mcp — e.g. `make BIN_DIR=dist`),
# GO, GOFMT, LDFLAGS, ARGS (server flags), WORKDIR (relay).

GO      ?= go
GOFMT   ?= gofmt
BIN_DIR ?= .
LDFLAGS ?=
ARGS    ?=
WORKDIR ?= $(CURDIR)

BUTLER     := $(BIN_DIR)/butler
BUTLER_MCP := $(BIN_DIR)/butler-mcp

# Butler's version is a const in each main package, so there is nothing to
# inject via -ldflags today; LDFLAGS is here for the day that changes.
LDFLAG_ARG = $(if $(strip $(LDFLAGS)),-ldflags "$(LDFLAGS)")

# The target names double as file names at the root, so keep them phony:
# a real file target with no prerequisites would look up to date and skip the
# build. Go's own build cache already makes repeated builds cheap.
.PHONY: all build butler butler-mcp test vet check fmt fmt-check run relay install clean help
.DEFAULT_GOAL := build

all: build

build: butler butler-mcp

butler:
	$(GO) build $(LDFLAG_ARG) -o $(BUTLER) ./cmd/butler

butler-mcp:
	$(GO) build $(LDFLAG_ARG) -o $(BUTLER_MCP) ./cmd/butler-mcp

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

check: vet test

# gofmt -l only lists, so this fails without touching the files.
fmt-check:
	@out=$$($(GOFMT) -l cmd internal); \
	if [ -n "$$out" ]; then echo "needs gofmt:"; echo "$$out"; exit 1; fi

fmt:
	$(GOFMT) -w cmd internal

# From source, with the server's flags: make run ARGS='--listen 127.0.0.1:9000'
run:
	$(GO) run ./cmd/butler serve $(ARGS)

# The codex-side push daemon (one per workdir); state file needs a workdir.
relay:
	$(GO) run ./cmd/butler-mcp relay --workdir $(WORKDIR)

install:
	$(GO) install ./cmd/butler ./cmd/butler-mcp

clean:
	rm -f $(BUTLER) $(BUTLER_MCP)

help:
	@echo "make [target]   (default: build)"
	@echo "  build       build $(BUTLER) and $(BUTLER_MCP)"
	@echo "  butler      build the server only"
	@echo "  butler-mcp  build the MCP server only"
	@echo "  check       vet + test"
	@echo "  test        the test suite"
	@echo "  fmt         gofmt -w cmd internal"
	@echo "  fmt-check   fail if anything needs gofmt"
	@echo "  run         go run ./cmd/butler serve [ARGS=...]"
	@echo "  relay       go run ./cmd/butler-mcp relay --workdir [WORKDIR=...]"
	@echo "  install     go install both commands"
	@echo "  clean       remove the built binaries"