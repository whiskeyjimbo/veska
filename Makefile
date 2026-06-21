.DEFAULT_GOAL := all

BINDIR := bin
VESKA_BIN      := $(BINDIR)/veska
DAEMON_BIN      := $(BINDIR)/veska-daemon
MCP_BIN         := $(BINDIR)/veska-mcp
LAYERCHECK_BIN  := $(BINDIR)/layercheck

# SQLite driver is `github.com/mattn/go-sqlite3` (cgo +
# sqlite_fts5 are mandatory; the lexical-fallback path uses FTS5 virtual
# tables). tree-sitter requires cgo anyway, so there is no no-cgo build.
SQLITE_TAGS    ?= sqlite_fts5
SQLITE_CGO_ENV ?= CGO_ENABLED=1

.PHONY: all build build-small build-fat fetch-embed-assets notices install release-archive changelog test lint vet layercheck fatfile-ratchet noidleak cliparity clean loadtest test-mcp test-mcp-deep test-mcp-bootstrap eval-recall eval-recall-projection eval-autolink-fp eval-neardup-threshold eval-revalidate-bench eval-wake-latency eval-queue-fuzz eval-embed-throughput eval-embedder-bench eval-embed-models eval-embed-models-full eval-embed-models-condense eval-embed-models-fuse eval-dbbench eval-dbbench-cgo docs-gen docs-check

# `all` uses build-small to keep the test loop fast - the model2vec assets
# add a network fetch + ~62MB to every CI/dev run. End-user packaging
# (`make build`) ships fat.
all: build-small test vet lint layercheck fatfile-ratchet noidleak cliparity

# `build` : default to the fat binary - model2vec embedded -
# so a clean clone + `make build` produces a usable veska without the
# install-model2vec dance. Size-sensitive callers use `build-small`.
build: fetch-embed-assets
	$(SQLITE_CGO_ENV) go build -tags "embed_model $(SQLITE_TAGS)" -o $(VESKA_BIN) ./cmd/veska
	ln -sf veska $(DAEMON_BIN)
	ln -sf veska $(MCP_BIN)
	go build -o $(LAYERCHECK_BIN) ./tools/lint/layercheck/cmd

# build-small : thin binary, no embedded model. Veska elects
# the low-quality static-v2 fallback at first boot unless the user runs
# `veska install model2vec`. Intended for CI / container layers where the
# ~62MB embed bloat matters more than first-run UX.
#
# 'build' and 'build-small' both produce $(VESKA_BIN), so
# the file-target rule for $(VESKA_BIN) was skipped when running build-
# small after build (and vice versa) - make sees an existing artifact and
# does nothing, leaving the user with the wrong-mode binary they already
# had. Recipe now removes the artifact unconditionally and invokes the
# thin go build directly so the mode the user asked for is the mode they
# get, every time.
build-small: $(LAYERCHECK_BIN)
	@rm -f $(VESKA_BIN) $(DAEMON_BIN) $(MCP_BIN)
	$(SQLITE_CGO_ENV) go build -tags "$(SQLITE_TAGS)" -o $(VESKA_BIN) ./cmd/veska
	ln -sf veska $(DAEMON_BIN)
	ln -sf veska $(MCP_BIN)

# build-fat: deprecated alias for `build` . Kept for one
# release so muscle-memory keeps working; remove next cycle.
build-fat: build
	@echo "note: 'make build-fat' is now an alias for 'make build'; update scripts." >&2

# Embed-asset dir for fat builds . Contents are .gitignore'd -
# the ~62MB weights are never committed.
EMBED_ASSET_DIR := internal/infrastructure/embedding/model2vec/assets/potion-code-16M

# fetch-embed-assets: populate the //go:embed asset dir using the SAME
# pinned ModelSpec + sha verification `veska install model2vec` uses, so
# there is one source of truth for the model revision. Runs the installer
# from current source (go run) - not a prebuilt bin, which may be stale -
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

# notices: regenerate THIRD_PARTY_NOTICES from the live dependency graph. Scans
# with every tag that ships in a binary (embed_model + hnsw_native + sqlite_fts5)
# so fat-build deps (usearch) are covered. Auto-detected license texts come from
# go-licenses; the model weights + sqlite-vec (which go-licenses can't classify)
# live in manual-notices.txt and are appended. Run after any dependency bump.
# Requires: go install github.com/google/go-licenses@latest
notices:
	GOFLAGS="-tags=embed_model,hnsw_native,sqlite_fts5" go-licenses report ./... \
		--template tools/licensing/notices.tpl \
		--ignore github.com/whiskeyjimbo/veska \
		--ignore github.com/asg017/sqlite-vec-go-bindings \
		> THIRD_PARTY_NOTICES
	cat tools/licensing/manual-notices.txt >> THIRD_PARTY_NOTICES
	@echo "THIRD_PARTY_NOTICES regenerated ($$(wc -l < THIRD_PARTY_NOTICES) lines)"

$(VESKA_BIN):
	$(SQLITE_CGO_ENV) go build -tags "$(SQLITE_TAGS)" -o $@ ./cmd/veska

$(LAYERCHECK_BIN):
	go build -o $@ ./tools/lint/layercheck/cmd

# build-sizes: measure thin and fat binary sizes so
# README numbers stay in sync with reality. Cleans bin/veska between
# runs because $(VESKA_BIN) is the same path for both modes - without
# the clean, make sees an existing artifact and skips the rebuild.
.PHONY: build-sizes
build-sizes:
	@rm -f $(VESKA_BIN) $(DAEMON_BIN) $(MCP_BIN)
	@$(MAKE) build > /dev/null 2>&1
	@fat=$$(stat -L -c%s $(VESKA_BIN)); \
	rm -f $(VESKA_BIN) $(DAEMON_BIN) $(MCP_BIN); \
	$(MAKE) build-small > /dev/null 2>&1; \
	thin=$$(stat -L -c%s $(VESKA_BIN)); \
	printf 'thin: %d bytes (%d MB)\nfat:  %d bytes (%d MB)\ndelta:%d bytes (%d MB)\n' \
		$$thin $$((thin/1024/1024)) \
		$$fat  $$((fat/1024/1024)) \
		$$((fat-thin)) $$(((fat-thin)/1024/1024))

# install : copy the just-built fat binaries into the user's
# bin dir via scripts/install.sh. Mirrors the install.sh path inside the
# release tarball so the local-build experience matches the distributed
# one. Destination override via $VESKA_INSTALL_DIR; defaults to
# ~/.local/bin. Run `make build` first (this target depends on it).
install: build
	scripts/install.sh

# release-archive : produce a tarball at
# dist/veska-<version>-<os>-<arch>.tar.gz containing the fat binaries +
# install.sh + a top-level README. A user downloading the tarball runs
# `./install.sh` and gets the same outcome as a developer running
# `make install` from a clone.
#
# Version source is the same `git describe` that produced shortVersion()
# in cmd/veska/version.go - kept inside the recipe so a dirty tree
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

# changelog: regenerate CHANGELOG.md from conventional commits via git-cliff.
# The release workflow builds per-tag release notes the same way
# (`git-cliff --latest`); this target writes the full-history file. Requires
# git-cliff on PATH (https://git-cliff.org - `cargo install git-cliff` or
# `brew install git-cliff`).
changelog:
	@command -v git-cliff >/dev/null || { echo "git-cliff not found - see https://git-cliff.org" >&2; exit 1; }
	git-cliff --config cliff.toml -o CHANGELOG.md
	@printf 'wrote CHANGELOG.md\n'

test:
	$(SQLITE_CGO_ENV) go test -tags "$(SQLITE_TAGS)" ./...

# test-race: race-detector run with package parallelism pinned to 1.
# TestToolCoverage builds a full isolated harness (sqlite + memvec + static
# embedder + cold-scan) per leaf; under `go test -race ./...` the detector
# inflates peak RSS ~5-10x, and when this heavy package overlaps with others it
# can spike memory enough to panic (observed once in ~16 runs). The subtests are
# already sequential and the harness has no logic race - the contention is purely
# cross-package, so `-p 1` serializes the test binaries and makes race runs
# deterministic at the cost of wall time.
test-race:
	$(SQLITE_CGO_ENV) go test -race -p 1 -tags "$(SQLITE_TAGS)" ./...

# tool-test: run the in-process MCP tool-coverage suite. Narrow to
# a family and/or tool with FAMILY=/TOOL= which append to the -run subtest path,
# e.g. `make tool-test FAMILY=graph TOOL=eng_get_node`.
TOOLTEST_RUN := TestToolCoverage
ifneq ($(FAMILY),)
TOOLTEST_RUN := $(TOOLTEST_RUN)/$(FAMILY)
ifneq ($(TOOL),)
TOOLTEST_RUN := $(TOOLTEST_RUN)/$(TOOL)
endif
endif
tool-test:
	$(SQLITE_CGO_ENV) go test -tags "$(SQLITE_TAGS)" -run '$(TOOLTEST_RUN)' ./internal/cli/daemon/...

# tool-test-e2e: on-demand socket end-to-end harness. Build-tag
# gated behind `socket_e2e` so it stays out of the default test path; it drives
# the real daemon Unix-socket JSON-RPC round-trip through the registry.
tool-test-e2e:
	$(SQLITE_CGO_ENV) go test -tags "$(SQLITE_TAGS) socket_e2e" -run TestSocketE2E ./internal/cli/daemon/...

vet:
	$(SQLITE_CGO_ENV) go vet -tags "$(SQLITE_TAGS)" ./...

lint:
	golangci-lint run ./cmd/... ./internal/...
	$(MAKE) lint-size

# lint-size: enforce the <=50 LOC / <=15 cyclomatic / <=5 args bar on CHANGED
# code only. LINT_SIZE_BASE is the ref the diff is taken
# against - default 'main'; override (e.g. LINT_SIZE_BASE=origin/main) in CI.
LINT_SIZE_BASE ?= main
lint-size:
	golangci-lint run -c .golangci-size.yml --new-from-merge-base=$(LINT_SIZE_BASE) ./cmd/... ./internal/...

layercheck: $(LAYERCHECK_BIN)
	$(LAYERCHECK_BIN) .

# fatfile-ratchet: per-FILE total-LOC ratchet. Reads the
# checked-in inventory (tools/lint/fatfiles/inventory.txt) of already-oversized
# files and fails if any has GROWN past its recorded ceiling. Complements
# lint-size (per-function, changed-code-only) by shrinking the fat-file backlog
# over time instead of grandfathering it. Lower an entry only after the file
# actually shrinks; never raise it.
fatfile-ratchet:
	go run ./tools/lint/fatfiles/cmd

# noidleak: fail when bd issue IDs  appear in user-visible Go
# string literals - flag descriptions, fmt strings, MCP tool descriptions
# . Comments are allowed.
noidleak:
	go run ./tools/lint/noidleak

# cliparity: every registered MCP tool must either be wrapped by a
# cobra subcommand (declared in tools/lint/cliparity/wrapped.txt) or be
# marked CLIExempt on its ToolSpec literal.
cliparity:
	go run ./tools/lint/cliparity

# persona-parity: every registered eng_* MCP tool must be EXERCISED by a
# tests/mcp test - a persona workflow or the per-tool suite - or listed with a
# reason in tools/lint/personaparity/parked.txt. The "test ALL functionality"
# guarantee for the MCP surface; a new untested tool turns this red.
persona-parity:
	go run ./tools/lint/personaparity

clean:
	rm -f $(VESKA_BIN) $(DAEMON_BIN) $(MCP_BIN) $(LAYERCHECK_BIN)
	rm -rf dist site

# docs-gen: regenerate the manual's derived reference pages from live source.
# CLI reference comes from the in-process cobra tree (`veska gendocs`); the
# config env-var reference is AST-extracted from internal/platform/config; the
# MCP tools reference is rendered from the production tool registry via a
# docsgen-tagged test (reuses registerMCPTools - same surface as tools/list).
DOCS_REF := docs/manual/reference
docs-gen:
	go run ./cmd/veska gendocs $(DOCS_REF)/cli.md
	go run ./tools/docgen config $(DOCS_REF)/config.md
	MCP_DOCS_OUT="$(abspath $(DOCS_REF)/mcp-tools.md)" \
	  $(SQLITE_CGO_ENV) go test -tags "$(SQLITE_TAGS) docsgen" \
	  -run TestGenerateMCPToolsDoc ./internal/cli/daemon/ -count=1

# docs-check: fail if the committed reference pages are stale. CI runs this so
# a code change that shifts the CLI/config surface without `make docs-gen`
# breaks the build.
docs-check: docs-gen
	@git diff --exit-code -- $(DOCS_REF) \
	  || { echo "ERROR: generated docs are stale. Run 'make docs-gen' and commit."; exit 1; }

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

# test-persona: persona-shaped end-to-end workflows (junior / senior / agent)
# over a synthetic repo, each spawning its own daemon in a tmp VESKA_HOME
# (~12s total). Runs the persona-parity coverage gate FIRST (fast, no daemon)
# so a tool with no test fails before the slow suite. Needs the three binaries;
# no Ollama (model2vec). Maps to SOLO-02 - see tests/mcp/PERSONA.md.
test-persona: persona-parity $(VESKA_BIN) $(DAEMON_BIN) $(MCP_BIN)
	PYTHONPATH=. python3 -m pytest tests/mcp -v -s -m persona

# persona-verify-capture: the repeatable capture driver behind /persona-verify.
# Enumerates the LIVE tool surface (tools/list) and drives every tool over the
# synthetic fixture, dumping verbatim request/response for the model to JUDGE
# (capture, not pass/fail asserts - the one hard check is that no live tool is
# silently skipped). Needs the three binaries; no Ollama. See the
# /persona-verify skill, which reads this transcript.
persona-verify-capture: $(VESKA_BIN) $(DAEMON_BIN) $(MCP_BIN)
	PYTHONPATH=. python3 -m pytest tests/mcp/persona_verify_driver.py -v -s -m persona_verify

# loadtest: manual-only - collates M1 exit-gate RESULTS.md files and emits tools/loadtest/REPORT.md.
# Not included in `all`. Exit 0=all-pass, 1=fail, 2=pending.
loadtest:
	go build -tags loadtest -o /tmp/veska-loadtest ./tools/loadtest/driver/
	/tmp/veska-loadtest

# eval-recall: semantic-search recall@10 + p95 harness (m3.03.3). Quick mode
# (RECALL_POP=1000, fake embedder) is the default and runs in ~1s. Override
# RECALL_POP for larger runs; see tools/loadtest/recall/README.md.
eval-recall:
	RECALL_POP=$${RECALL_POP:-1000} $(SQLITE_CGO_ENV) go test -tags "eval $(SQLITE_TAGS)" -run TestRecall ./tools/loadtest/recall/ -v

# eval-recall-projection: recall@10 + p95 sweep over embed-text PROJECTION
# variants (baseline / +signature / +snippet / +both). The corpus is built
# from node-shaped inputs run through the production domain.EmbedText
# projection, so a variant change moves the measured recall number.
# Requires a reachable Ollama; skips cleanly if absent. Override RECALL_POP
# (default 1000) and RECALL_PROJECTION_VARIANT to restrict to one variant.
# A full 4-variant sweep at pop=1000 is reference-laptop work - raise the
# timeout accordingly. See tools/loadtest/recallprojection/README.md.
eval-recall-projection:
	RECALL_POP=$${RECALL_POP:-1000} $(SQLITE_CGO_ENV) go test -tags "eval $(SQLITE_TAGS)" -run TestRecallProjectionSweep ./tools/loadtest/recallprojection/ -v -timeout=3600s

# eval-autolink-fp: auto-link false-positive harness (m3.04.4). Quick mode
# (AUTOLINK_POP=1000, fake embedder) is the default and runs in ~1s. Override
# AUTOLINK_POP / AUTOLINK_THRESHOLD / AUTOLINK_TOPK for sweeps; see
# tools/loadtest/autolink/README.md.
eval-autolink-fp:
	AUTOLINK_POP=$${AUTOLINK_POP:-1000} $(SQLITE_CGO_ENV) go test -tags "eval $(SQLITE_TAGS)" -run TestAutolinkFP ./tools/loadtest/autolink/ -v

# eval-neardup-threshold: near-duplicate threshold calibration.
# Embeds a curated corpus of real Go functions + mechanical near-dup variants
# through model2vec (potion-code-16M, compiled in via embed_model - no service)
# and, when Ollama is reachable, nomic-embed-text; reports per-tier score
# distributions (neardup / related / unrelated) so DefaultNearThreshold is set
# from data. See tools/loadtest/neardup/.
eval-neardup-threshold:
	go test -tags "eval embed_model" -run TestNearDupThreshold ./tools/loadtest/neardup/ -v -count=1

# eval-revalidate-bench: revalidation wall-time harness against a synthetic
# 10k-node / 10k-edge / 3k-finding commit (m3.05.4). Asserts the M3 exit-gate
# target (< 60s). No quick-mode override - the gate IS the 10k case. See
# tools/loadtest/revalidate/README.md.
eval-revalidate-bench:
	$(SQLITE_CGO_ENV) go test -tags "eval $(SQLITE_TAGS)" -run TestRevalidateBench ./tools/loadtest/revalidate/ -v -count=1 -timeout=120s

# eval-wake-latency - wake-reconcile sweep latency gate.
# Times git.WakeReconciler's no-change mtime/size/prefix walk over a
# synthetic tree and asserts the SOLO-03 §5.2 NFR: typical-repo p95 < 500ms
# and a single >50k-file sweep < 5s. The git package needs no sqlite tags.
# Override WAKE_FILES / WAKE_FILES_LARGE. See tools/loadtest/wakelatency/README.md.
eval-wake-latency:
	go test -tags eval -run TestWakeLatency ./tools/loadtest/wakelatency/ -v -count=1 -timeout=120s

# eval-dbbench: compare Go SQLite drivers (mattn, zombiezen)
# against veska's storage workloads. Pure-Go variant (zombiezen only).
# Writes tools/loadtest/dbbench/RESULTS.md. See README.
eval-dbbench:
	go test -tags=eval -run TestDBBench ./tools/loadtest/dbbench/ -v -count=1 -timeout=600s

# eval-dbbench-cgo: same harness, including mattn (requires cgo + sqlite_fts5).
eval-dbbench-cgo:
	CGO_ENABLED=1 go test -tags="eval sqlite_fts5" -run TestDBBench ./tools/loadtest/dbbench/ -v -count=1 -timeout=600s

# eval-queue-fuzz: M3 gate-5 - drive N synthetic promotions through Promoter and
# assert all three M3 work_kind lanes (embed/auto_link/revalidate) drain to done.
# Override QUEUEFUZZ_PROMOTIONS / QUEUEFUZZ_BUDGET_MS to tune. See
# tools/loadtest/queuefuzz/README.md.
eval-queue-fuzz:
	QUEUEFUZZ_PROMOTIONS=$${QUEUEFUZZ_PROMOTIONS:-100} $(SQLITE_CGO_ENV) go test -tags "eval $(SQLITE_TAGS)" -run TestQueueFuzz ./tools/loadtest/queuefuzz/ -v -timeout=120s

# eval-embed-throughput: M3 gate-1 - drive embedder.Worker against real Ollama
# for a measurement window; assert throughput >= 5 emb/s (gate-1 lower bound).
# Override EMBED_BENCH_DURATION_S / EMBED_BENCH_SEED_N / VESKA_OLLAMA_URL /
# VESKA_EMBED_MODEL. Skips if Ollama is unreachable. See README.
eval-embed-throughput:
	EMBED_BENCH_DURATION_S=$${EMBED_BENCH_DURATION_S:-60} $(SQLITE_CGO_ENV) go test -tags "eval $(SQLITE_TAGS)" -run TestEmbedderThroughput ./tools/loadtest/embedder/ -v -timeout=180s

# eval-embedder-bench: per-embed throughput + load-cost micro-benchmarks
# across the election ladder (static-v2 / model2vec disk / model2vec
# embedded) - informed the fat/thin packaging decision .
# Disk arms skip without an installed model; the embedded arm needs the
# fat build tag, so this target builds with `-tags 'eval embed_model'`
# (run `make build-fat` once so the embed assets exist). See README.
eval-embedder-bench:
	go test -tags='eval embed_model' -run '^$$' -bench 'Load|Embed' -benchmem ./tools/loadtest/embedder/

# eval-embed-models: phased benchmark of embedding model variants over
# real codebase corpora. Used to inform hi5's defaults and publish a
# comparison table . Default runs the model2vec subset only
# - no external service required. See env knobs at the top of
# embed_models_test.go.
eval-embed-models:
	go test -tags=eval -run TestEmbedModelsBenchmark ./tools/loadtest/embed_models/ -v -timeout=300s

# eval-embed-models-full: same harness as eval-embed-models, but adds
# the Ollama model set (nomic-embed-text, bge-m3, snowflake-arctic-embed,
# mxbai-embed-large). Requires Ollama running and the models pulled
# via `ollama pull <name>`. The harness probes /api/tags once at start
# and gracefully drops the Ollama subset if unreachable rather than
# failing - keeps the contributor experience smooth.
eval-embed-models-full:
	EMBED_BENCH_INCLUDE_OLLAMA=1 go test -tags=eval -run TestEmbedModelsBenchmark ./tools/loadtest/embed_models/ -v -timeout=3600s

# eval-embed-models-condense: same as eval-embed-models but with the
# condensation axis enabled (oo4q.2). Each (model × corpus) cell gets
# a second condensed-vec embed; results.json + the published markdown
# table emit a Lift column. Adds ~3min to a full model2vec sweep.
# Knobs: EMBED_BENCH_CONDENSE_K (default 5) - top-K pieces kept per doc.
#        EMBED_BENCH_CONDENSE_MIN_LEN (default 500) - skip docs shorter.
# DO NOT combine with EMBED_BENCH_INCLUDE_OLLAMA - Ollama per-piece
# embeds would balloon runtime to hours.
eval-embed-models-condense:
	EMBED_BENCH_CONDENSE=on go test -tags=eval -run TestEmbedModelsBenchmark ./tools/loadtest/embed_models/ -v -timeout=1200s

# eval-embed-models-fuse: dual-model fusion bench . Embeds
# every doc with TWO model2vec variants (defaults: potion-code-16M as
# the code-side, potion-base-32M as the prose-side) and compares four
# ranking strategies on the same headline GT: code-only, prose-only,
# concat (mean of the two cosines), RRF (reciprocal rank fusion).
# Output: tools/loadtest/embed_models/out/fuse-results.json + a fusion
# section appended to docs/operations/embedder-benchmarks.md. Knobs:
# FUSE_MODEL_CODE, FUSE_MODEL_PROSE, FUSE_RRF_K, EMBED_BENCH_MAX_DOCS.
eval-embed-models-fuse:
	go test -tags=eval -run TestEmbedModelsFusion ./tools/loadtest/embed_models/ -v -timeout=600s

# eval-review-timing: M5 exit-gate-5 - drive the review Handler over a synthetic
# ~100-file commit against a real Ollama and report the wall-clock time budget.
# Measurement only (no pass/fail gate). Override REVIEW_TIMING_FILE_N /
# VESKA_OLLAMA_URL / VESKA_REVIEW_MODEL. Skips if Ollama is unreachable. See
# tools/loadtest/reviewtiming/README.md.
eval-review-timing:
	REVIEW_TIMING_FILE_N=$${REVIEW_TIMING_FILE_N:-100} go test -tags=eval -run TestReviewTiming ./tools/loadtest/reviewtiming/ -v -timeout=12000s

# eval-share-vs-regenerate: Times the
# parse + embed pipeline stages on a real library and reports the breakeven
# bandwidth per derived-artifact family (the link speed above which sharing beats
# local regeneration). Runs with no external service (elected embedder is
# model2vec if installed, else zero-dep static-v2). Knobs: SHARE_REGEN_ROOT
# (default this repo's internal/), SHARE_REGEN_MAX_DOCS (default 5000). Set
# VESKA_EMBEDDER=ollama (Ollama up) for the heavy-embedder crossover data point.
eval-share-vs-regenerate:
	go test -tags=eval -run TestShareVsRegenerate ./tools/loadtest/share-vs-regenerate/ -v -timeout=1800s

# eval-token-efficiency: produce the semble-shaped
# "tokens saved vs grep+read" figure, paired with recall@10 on the same
# corpus. Pure-Go simulation (no rg subprocess); cl100k_base tokenizer.
# Writes tools/loadtest/tokenefficiency/results.json + a one-line
# summary. Knob: TOKEFF_NODES_PER_CLUSTER (default 24) tunes how big
# each cluster file gets - larger files widen the savings bracket.
eval-token-efficiency:
	$(SQLITE_CGO_ENV) go test -tags "eval $(SQLITE_TAGS)" -run '^TestTokenEfficiency$$' ./tools/loadtest/tokenefficiency/ -v -count=1 -timeout=120s

# eval-token-efficiency-multirepo: the WEDGE headline.
# Partitions the synthcorpus across N repos and measures veska's
# cross-repo fanout + global RRF vs
# grep+read across every repo's file tree. Writes
# tools/loadtest/tokenefficiency/results-multirepo.json + a multi-repo
# one-line summary. Knobs: TOKEFF_REPOS (default 5), TOKEFF_NODES_PER_CLUSTER.
eval-token-efficiency-multirepo:
	$(SQLITE_CGO_ENV) go test -tags "eval $(SQLITE_TAGS)" -run TestTokenEfficiencyMultiRepo ./tools/loadtest/tokenefficiency/ -v -count=1 -timeout=180s
