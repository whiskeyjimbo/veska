.DEFAULT_GOAL := all

BINDIR := bin
VESKA_BIN      := $(BINDIR)/veska
DAEMON_BIN      := $(BINDIR)/veska-daemon
MCP_BIN         := $(BINDIR)/veska-mcp
LAYERCHECK_BIN  := $(BINDIR)/layercheck

.PHONY: all build test lint vet layercheck clean loadtest eval-recall eval-recall-projection eval-autolink-fp eval-revalidate-bench eval-queue-fuzz eval-embed-throughput

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

# eval-recall-projection: recall@10 + p95 sweep over embed-text PROJECTION
# variants (baseline / +signature / +snippet / +both). The corpus is built
# from node-shaped inputs run through the production domain.EmbedText
# projection, so a variant change moves the measured recall number.
# Requires a reachable Ollama; skips cleanly if absent. Override RECALL_POP
# (default 1000) and RECALL_PROJECTION_VARIANT to restrict to one variant.
# A full 4-variant sweep at pop=1000 is reference-laptop work — raise the
# timeout accordingly. See tools/loadtest/recallprojection/README.md.
eval-recall-projection:
	RECALL_POP=$${RECALL_POP:-1000} go test -tags=eval -run TestRecallProjectionSweep ./tools/loadtest/recallprojection/ -v -timeout=3600s

# eval-autolink-fp: auto-link false-positive harness (m3.04.4). Quick mode
# (AUTOLINK_POP=1000, fake embedder) is the default and runs in ~1s. Override
# AUTOLINK_POP / AUTOLINK_THRESHOLD / AUTOLINK_TOPK for sweeps; see
# tools/loadtest/autolink/README.md.
eval-autolink-fp:
	AUTOLINK_POP=$${AUTOLINK_POP:-1000} go test -tags=eval -run TestAutolinkFP ./tools/loadtest/autolink/ -v

# eval-revalidate-bench: revalidation wall-time harness against a synthetic
# 10k-node / 10k-edge / 3k-finding commit (m3.05.4). Asserts the M3 exit-gate
# target (< 60s). No quick-mode override — the gate IS the 10k case. See
# tools/loadtest/revalidate/README.md.
eval-revalidate-bench:
	go test -tags=eval -run TestRevalidateBench ./tools/loadtest/revalidate/ -v -count=1 -timeout=120s

# eval-queue-fuzz: M3 gate-5 — drive N synthetic promotions through Promoter and
# assert all three M3 work_kind lanes (embed/auto_link/revalidate) drain to done.
# Override QUEUEFUZZ_PROMOTIONS / QUEUEFUZZ_BUDGET_MS to tune. See
# tools/loadtest/queuefuzz/README.md.
eval-queue-fuzz:
	QUEUEFUZZ_PROMOTIONS=$${QUEUEFUZZ_PROMOTIONS:-100} go test -tags=eval -run TestQueueFuzz ./tools/loadtest/queuefuzz/ -v -timeout=120s

# eval-embed-throughput: M3 gate-1 — drive embedder.Worker against real Ollama
# for a measurement window; assert throughput >= 5 emb/s (gate-1 lower bound).
# Override EMBED_BENCH_DURATION_S / EMBED_BENCH_SEED_N / VESKA_OLLAMA_URL /
# VESKA_EMBED_MODEL. Skips if Ollama is unreachable. See README.
eval-embed-throughput:
	EMBED_BENCH_DURATION_S=$${EMBED_BENCH_DURATION_S:-60} go test -tags=eval -run TestEmbedderThroughput ./tools/loadtest/embedder/ -v -timeout=180s

# eval-review-timing: M5 exit-gate-5 — drive the review Handler over a synthetic
# ~100-file commit against a real Ollama and report the wall-clock time budget.
# Measurement only (no pass/fail gate). Override REVIEW_TIMING_FILE_N /
# VESKA_OLLAMA_URL / VESKA_REVIEW_MODEL. Skips if Ollama is unreachable. See
# tools/loadtest/reviewtiming/README.md.
eval-review-timing:
	REVIEW_TIMING_FILE_N=$${REVIEW_TIMING_FILE_N:-100} go test -tags=eval -run TestReviewTiming ./tools/loadtest/reviewtiming/ -v -timeout=12000s
