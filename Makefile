.DEFAULT_GOAL := all

BINDIR := bin
ENGRAM_BIN    := $(BINDIR)/engram
DAEMON_BIN    := $(BINDIR)/engram-daemon
MCP_BIN       := $(BINDIR)/engram-mcp

.PHONY: all build test lint vet clean

all: build test vet lint

build: $(ENGRAM_BIN) $(DAEMON_BIN) $(MCP_BIN)

$(ENGRAM_BIN):
	go build -o $@ ./cmd/engram

$(DAEMON_BIN):
	go build -o $@ ./cmd/engram-daemon

$(MCP_BIN):
	go build -o $@ ./cmd/engram-mcp

test:
	go test ./...

vet:
	go vet ./...

lint:
	golangci-lint run ./cmd/... ./internal/...

clean:
	rm -f $(ENGRAM_BIN) $(DAEMON_BIN) $(MCP_BIN)
