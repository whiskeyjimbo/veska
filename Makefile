.DEFAULT_GOAL := all

BINDIR := bin
ENGRAM_BIN      := $(BINDIR)/engram
DAEMON_BIN      := $(BINDIR)/engram-daemon
MCP_BIN         := $(BINDIR)/engram-mcp
LAYERCHECK_BIN  := $(BINDIR)/layercheck

.PHONY: all build test lint vet layercheck clean loadtest

all: build test vet lint layercheck

build: $(ENGRAM_BIN) $(DAEMON_BIN) $(MCP_BIN) $(LAYERCHECK_BIN)

$(ENGRAM_BIN):
	go build -o $@ ./cmd/engram

$(DAEMON_BIN):
	go build -o $@ ./cmd/engram-daemon

$(MCP_BIN):
	go build -o $@ ./cmd/engram-mcp

$(LAYERCHECK_BIN):
	go build -o $@ ./tools/lint/layercheck/cmd

test:
	go test ./...

vet:
	go vet ./...

lint:
	golangci-lint run ./cmd/... ./internal/...

layercheck: $(LAYERCHECK_BIN)
	$(LAYERCHECK_BIN) .

clean:
	rm -f $(ENGRAM_BIN) $(DAEMON_BIN) $(MCP_BIN) $(LAYERCHECK_BIN)

# loadtest: manual-only — collates M1 exit-gate RESULTS.md files and emits tools/loadtest/REPORT.md.
# Not included in `all`. Exit 0=all-pass, 1=fail, 2=pending.
loadtest:
	go build -tags loadtest -o /tmp/engram-loadtest ./tools/loadtest/driver/
	/tmp/engram-loadtest
