.DEFAULT_GOAL := all

BINDIR := bin
VESKA_BIN      := $(BINDIR)/veska
DAEMON_BIN      := $(BINDIR)/veska-daemon
MCP_BIN         := $(BINDIR)/veska-mcp
LAYERCHECK_BIN  := $(BINDIR)/layercheck

# solov2-jkgp: production SQLite driver is `github.com/mattn/go-sqlite3`
# (cgo + sqlite_fts5 are mandatory; the lexical-fallback path uses FTS5
# virtual tables). Set on every `go build`/`go test` that touches the
# production sqlite layer. To opt back into the pure-Go modernc driver
# (e.g. for a CGO_ENABLED=0 cross-compile), set SQLITE_TAGS=sqlite_modernc
# and SQLITE_CGO_ENV='' from the command line.
SQLITE_TAGS    ?= sqlite_fts5
SQLITE_CGO_ENV ?= CGO_ENABLED=1

.PHONY: all build build-small build-fat fetch-embed-assets install release-archive test lint vet layercheck clean loadtest test-mcp test-mcp-deep test-mcp-bootstrap eval-recall eval-recall-projection eval-autolink-fp eval-revalidate-bench eval-queue-fuzz eval-embed-throughput eval-embedder-bench eval-embed-models eval-embed-models-full eval-embed-models-condense eval-embed-models-fuse eval-dbbench eval-dbbench-cgo

# `all` uses build-small to keep the test loop fast — the model2vec assets
# add a network fetch + ~62MB to every CI/dev run. End-user packaging
# (`make build`) ships fat.
all: build-small test vet lint layercheck

# `build` (solov2-sft7): default to the fat binary — model2vec embedded —
# so a clean clone + `make build` produces a usable veska without the
# install-model2vec dance. Size-sensitive callers use `build-small`.
build: fetch-embed-assets
	$(SQLITE_CGO_ENV) go build -tags "embed_model $(SQLITE_TAGS)" -o $(VESKA_BIN) ./cmd/veska
	ln -sf veska $(DAEMON_BIN)
	ln -sf veska $(MCP_BIN)
	go build -o $(LAYERCHECK_BIN) ./tools/lint/layercheck/cmd

# build-small (solov2-sft7): thin binary, no embedded model. Veska elects
# the low-quality static-v2 fallback at first boot unless the user runs
# `veska install model2vec`. Intended for CI / container layers where the
# ~62MB embed bloat matters more than first-run UX.
build-small: $(VESKA_BIN) $(LAYERCHECK_BIN)
	ln -sf veska $(DAEMON_BIN)
	ln -sf veska $(MCP_BIN)

# build-fat: deprecated alias for `build` (solov2-sft7). Kept for one
# release so muscle-memory keeps working; remove next cycle.
build-fat: build
	@echo "note: 'make build-fat' is now an alias for 'make build'; update scripts." >&2

# Embed-asset dir for fat builds (solov2-si1). Contents are .gitignore'd —
# the ~62MB weights are never committed.
EMBED_ASSET_DIR := internal/infrastructure/embedding/model2vec/assets/potion-code-16M

# fetch-embed-assets: populate the //go:embed asset dir using the SAME
# pinned ModelSpec + sha verification `veska install model2vec` uses, so
# there is one source of truth for the model revision. Runs the installer
# from current source (go run) — not a prebuilt bin, which may be stale —
# into a temp home, then copies the verified files into place. `set -e`
# aborts the recipe if the download/verify fails (rather than building a
# binary with no embedded model).
fetch-embed-assets:
	@set -e; tmp=$$(mktemp -d); \
	VESKA_HOME=$$tmp go run ./cmd/veska install model2vec; \
	mkdir -p $(EMBED_ASSET_DIR); \
	cp $$tmp/static-model/potion-code-16M/tokenizer.json $$tmp/static-model/potion-code-16M/model.safetensors $(EMBED_ASSET_DIR)/; \
	rm -rf $$tmp; \
	echo "embed assets ready in $(EMBED_ASSET_DIR)"

$(VESKA_BIN):
	$(SQLITE_CGO_ENV) go build -tags "$(SQLITE_TAGS)" -o $@ ./cmd/veska

$(LAYERCHECK_BIN):
	go build -o $@ ./tools/lint/layercheck/cmd

# install (solov2-cdw3): copy the just-built fat binaries into the user's
# bin dir via scripts/install.sh. Mirrors the install.sh path inside the
# release tarball so the local-build experience matches the distributed
# one. Destination override via $VESKA_INSTALL_DIR; defaults to
# ~/.local/bin. Run `make build` first (this target depends on it).
install: build
	scripts/install.sh

# release-archive (solov2-cdw3): produce a tarball at
# dist/veska-<version>-<os>-<arch>.tar.gz containing the fat binaries +
# install.sh + a top-level README. A user downloading the tarball runs
# `./install.sh` and gets the same outcome as a developer running
# `make install` from a clone.
#
# Version source is the same `git describe` that produced shortVersion()
# in cmd/veska/version.go — kept inside the recipe so a dirty tree
# still gets a meaningful tag and never silently ships unversioned.
RELEASE_GOOS    := $(shell go env GOOS)
RELEASE_GOARCH  := $(shell go env GOARCH)
RELEASE_VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
RELEASE_NAME    := veska-$(RELEASE_VERSION)-$(RELEASE_GOOS)-$(RELEASE_GOARCH)
RELEASE_DIR     := dist/$(RELEASE_NAME)
release-archive: build
	@rm -rf $(RELEASE_DIR) dist/$(RELEASE_NAME).tar.gz
	@mkdir -p $(RELEASE_DIR)/bin
	cp $(VESKA_BIN) $(RELEASE_DIR)/bin/
	ln -sf veska $(RELEASE_DIR)/bin/veska-daemon
	ln -sf veska $(RELEASE_DIR)/bin/veska-mcp
	cp scripts/install.sh $(RELEASE_DIR)/install.sh
	chmod +x $(RELEASE_DIR)/install.sh
	@printf 'veska %s\n\nThis archive contains the veska binaries (CLI, daemon, MCP shim)\nwith the model2vec embedder weights compiled in.\n\nInstall:\n  ./install.sh                # ~/.local/bin (default)\n  VESKA_INSTALL_DIR=/usr/local/bin sudo ./install.sh\n\nThen:\n  veska init -y && veska service install && veska service start\n\nDocs: https://github.com/whiskeyjimbo/veska\n' "$(RELEASE_VERSION)" > $(RELEASE_DIR)/README.txt
	cd dist && tar -czf $(RELEASE_NAME).tar.gz $(RELEASE_NAME)
	@printf 'release archive: dist/%s.tar.gz\n' "$(RELEASE_NAME)"

test:
	$(SQLITE_CGO_ENV) go test -tags "$(SQLITE_TAGS)" ./...

vet:
	$(SQLITE_CGO_ENV) go vet -tags "$(SQLITE_TAGS)" ./...

lint:
	golangci-lint run ./cmd/... ./internal/...

layercheck: $(LAYERCHECK_BIN)
	$(LAYERCHECK_BIN) .

clean:
	rm -f $(VESKA_BIN) $(DAEMON_BIN) $(MCP_BIN) $(LAYERCHECK_BIN)
	rm -rf dist

# test-mcp: black-box pytest harness against a running daemon. Needs:
#   - VESKA_HOME pointing at the daemon's data dir (or default ~/.veska)
#   - bin/veska-mcp built
#   - At least one repo registered (`veska repo add <path>`)
# Set VESKA_HOME inline, e.g.:
#   VESKA_HOME=/tmp/x make test-mcp
test-mcp: $(MCP_BIN)
	PYTHONPATH=. python3 -m pytest tests/mcp -v -s -m 'not deep and not bootstrap'

# test-mcp-deep: like test-mcp but also runs cross-validation tests that
# read the live SQLite directly and compare against MCP-returned shapes.
test-mcp-deep: $(MCP_BIN)
	PYTHONPATH=. python3 -m pytest tests/mcp -v -s -m 'not bootstrap'

# test-mcp-bootstrap: spawns its own daemon in a tmp VESKA_HOME and walks
# the full zero-state journey (~15s). Needs Ollama + the three binaries
# built. Doesn't touch the live daemon's state.
test-mcp-bootstrap: $(VESKA_BIN) $(DAEMON_BIN) $(MCP_BIN)
	PYTHONPATH=. python3 -m pytest tests/mcp -v -s -m bootstrap

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
	$(SQLITE_CGO_ENV) go test -tags "eval $(SQLITE_TAGS)" -run TestRevalidateBench ./tools/loadtest/revalidate/ -v -count=1 -timeout=120s

# eval-dbbench: solov2-6e5r — compare Go SQLite drivers (modernc, mattn,
# zombiezen) against veska's storage workloads. Pure-Go variant (modernc +
# zombiezen). Writes tools/loadtest/dbbench/RESULTS.md. See README.
eval-dbbench:
	go test -tags=eval -run TestDBBench ./tools/loadtest/dbbench/ -v -count=1 -timeout=600s

# eval-dbbench-cgo: same harness, including mattn (requires cgo + sqlite_fts5).
eval-dbbench-cgo:
	CGO_ENABLED=1 go test -tags="eval sqlite_fts5" -run TestDBBench ./tools/loadtest/dbbench/ -v -count=1 -timeout=600s

# eval-queue-fuzz: M3 gate-5 — drive N synthetic promotions through Promoter and
# assert all three M3 work_kind lanes (embed/auto_link/revalidate) drain to done.
# Override QUEUEFUZZ_PROMOTIONS / QUEUEFUZZ_BUDGET_MS to tune. See
# tools/loadtest/queuefuzz/README.md.
eval-queue-fuzz:
	QUEUEFUZZ_PROMOTIONS=$${QUEUEFUZZ_PROMOTIONS:-100} $(SQLITE_CGO_ENV) go test -tags "eval $(SQLITE_TAGS)" -run TestQueueFuzz ./tools/loadtest/queuefuzz/ -v -timeout=120s

# eval-embed-throughput: M3 gate-1 — drive embedder.Worker against real Ollama
# for a measurement window; assert throughput >= 5 emb/s (gate-1 lower bound).
# Override EMBED_BENCH_DURATION_S / EMBED_BENCH_SEED_N / VESKA_OLLAMA_URL /
# VESKA_EMBED_MODEL. Skips if Ollama is unreachable. See README.
eval-embed-throughput:
	EMBED_BENCH_DURATION_S=$${EMBED_BENCH_DURATION_S:-60} go test -tags=eval -run TestEmbedderThroughput ./tools/loadtest/embedder/ -v -timeout=180s

# eval-embedder-bench: per-embed throughput + load-cost micro-benchmarks
# across the election ladder (static-v2 / model2vec disk / model2vec
# embedded) — informed the fat/thin packaging decision (solov2-si1).
# Disk arms skip without an installed model; the embedded arm needs the
# fat build tag, so this target builds with `-tags 'eval embed_model'`
# (run `make build-fat` once so the embed assets exist). See README.
eval-embedder-bench:
	go test -tags='eval embed_model' -run '^$$' -bench 'Load|Embed' -benchmem ./tools/loadtest/embedder/

# eval-embed-models: phased benchmark of embedding model variants over
# real codebase corpora. Used to inform hi5's defaults and publish a
# comparison table (solov2-0k5h). Default runs the model2vec subset only
# — no external service required. See env knobs at the top of
# embed_models_test.go.
eval-embed-models:
	go test -tags=eval -run TestEmbedModelsBenchmark ./tools/loadtest/embed_models/ -v -timeout=300s

# eval-embed-models-full: same harness as eval-embed-models, but adds
# the Ollama model set (nomic-embed-text, bge-m3, snowflake-arctic-embed,
# mxbai-embed-large). Requires Ollama running and the models pulled
# via `ollama pull <name>`. The harness probes /api/tags once at start
# and gracefully drops the Ollama subset if unreachable rather than
# failing — keeps the contributor experience smooth.
eval-embed-models-full:
	EMBED_BENCH_INCLUDE_OLLAMA=1 go test -tags=eval -run TestEmbedModelsBenchmark ./tools/loadtest/embed_models/ -v -timeout=3600s

# eval-embed-models-condense: same as eval-embed-models but with the
# condensation axis enabled (oo4q.2). Each (model × corpus) cell gets
# a second condensed-vec embed; results.json + the published markdown
# table emit a Lift column. Adds ~3min to a full model2vec sweep.
# Knobs: EMBED_BENCH_CONDENSE_K (default 5) — top-K pieces kept per doc.
#        EMBED_BENCH_CONDENSE_MIN_LEN (default 500) — skip docs shorter.
# DO NOT combine with EMBED_BENCH_INCLUDE_OLLAMA — Ollama per-piece
# embeds would balloon runtime to hours.
eval-embed-models-condense:
	EMBED_BENCH_CONDENSE=on go test -tags=eval -run TestEmbedModelsBenchmark ./tools/loadtest/embed_models/ -v -timeout=1200s

# eval-embed-models-fuse: dual-model fusion bench (solov2-8hka). Embeds
# every doc with TWO model2vec variants (defaults: potion-code-16M as
# the code-side, potion-base-32M as the prose-side) and compares four
# ranking strategies on the same headline GT: code-only, prose-only,
# concat (mean of the two cosines), RRF (reciprocal rank fusion).
# Output: tools/loadtest/embed_models/out/fuse-results.json + a fusion
# section appended to docs/operations/embedder-benchmarks.md. Knobs:
# FUSE_MODEL_CODE, FUSE_MODEL_PROSE, FUSE_RRF_K, EMBED_BENCH_MAX_DOCS.
eval-embed-models-fuse:
	go test -tags=eval -run TestEmbedModelsFusion ./tools/loadtest/embed_models/ -v -timeout=600s

# eval-review-timing: M5 exit-gate-5 — drive the review Handler over a synthetic
# ~100-file commit against a real Ollama and report the wall-clock time budget.
# Measurement only (no pass/fail gate). Override REVIEW_TIMING_FILE_N /
# VESKA_OLLAMA_URL / VESKA_REVIEW_MODEL. Skips if Ollama is unreachable. See
# tools/loadtest/reviewtiming/README.md.
eval-review-timing:
	REVIEW_TIMING_FILE_N=$${REVIEW_TIMING_FILE_N:-100} go test -tags=eval -run TestReviewTiming ./tools/loadtest/reviewtiming/ -v -timeout=12000s
