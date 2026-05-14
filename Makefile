.DEFAULT_GOAL := all

BINDIR := bin
VESKA_BIN      := $(BINDIR)/veska
DAEMON_BIN      := $(BINDIR)/veska-daemon
MCP_BIN         := $(BINDIR)/veska-mcp
LAYERCHECK_BIN  := $(BINDIR)/layercheck

.PHONY: all build test lint vet layercheck clean loadtest eval-recall

all: build test vet lint layercheck

build: $(VESKA_BIN) $(DAEMON_BIN) $(MCP_BIN) $(LAYERCHECK_BIN)

$(VESKA_BIN):
	go build -o $@ ./cmd/veska

$(DAEMON_BIN):
	go build -o $@ ./cmd/veska-daemon

$(MCP_BIN):
	go build -o $@ ./cmd/veska-mcp

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
	rm -f $(VESKA_BIN) $(DAEMON_BIN) $(MCP_BIN) $(LAYERCHECK_BIN)

# loadtest: manual-only — collates M1 exit-gate RESULTS.md files and emits tools/loadtest/REPORT.md.
# Not included in `all`. Exit 0=all-pass, 1=fail, 2=pending.
loadtest:
	go build -tags loadtest -o /tmp/veska-loadtest ./tools/loadtest/driver/
	/tmp/veska-loadtest

# eval-recall: semantic-search recall@10 + p95 harness (m3.03.3). Quick mode
# (RECALL_POP=1000, fake embedder) is the default and runs in ~1s. Override
# RECALL_POP for larger runs; see tools/loadtest/recall/README.md.
eval-recall:
	RECALL_POP=$${RECALL_POP:-1000} go test -tags=eval -run TestRecall ./tools/loadtest/recall/ -v
