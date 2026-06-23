#!/usr/bin/env bash
# backend-matrix.sh - build the memvec-vs-usearch metrics matrix for the manual.
#
# Indexes a configured slate of Go repos (different sizes) into ONE isolated
# VESKA_HOME using the real daemon (usearch backend + model2vec, the baked-in
# defaults), then runs TestBackendMatrix to emit a markdown table comparing the
# two vector backends per repo: build time, query latency, autolink recall, RAM.
#
# Re-run after any vector-backend / quantization / embedder change to confirm
# the manual's numbers still hold. Clones are cached; pass --reindex to rebuild
# the index from scratch. Run on a QUIET box - build/latency are contention-
# sensitive and usearch's HNSW build is order-dependent.
#
#   ./tools/loadtest/usearchab/backend-matrix.sh [--reindex]
#
# Output: /tmp/backend-matrix.md
set -euo pipefail

REPO_SRC="/home/jrose/src/engram/solov2"
ROOT="/tmp/veska-backend-matrix"
HOME_DIR="$ROOT/home"
CLONES="/tmp/veska-metrics/repos"  # reused across runs; clones are cached
BIN="$ROOT/veska"
DAEMON_LINK="$ROOT/veska-daemon"
DB="$HOME_DIR/veska.db"
DAEMON_LOG="$ROOT/daemon.log"
DRAIN_TIMEOUT=1800
READY_TIMEOUT=60

# tier | label | source  (source = github "owner/name" or "LOCAL:<path>")
REPOS=(
  "S  go-git  go-git/go-git"
  "M  veska   LOCAL:$REPO_SRC"
  "M2 grpc-go grpc/grpc-go"
  "L  consul  hashicorp/consul"
)

note() { printf '\033[1;36m[matrix]\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31m[matrix] FATAL:\033[0m %s\n' "$*" >&2; exit 1; }

REINDEX=0
[ "${1:-}" = "--reindex" ] && REINDEX=1

q() { sqlite3 "$DB" "$1" 2>/dev/null || echo -1; }
pending_embeds() { q "SELECT COUNT(*) FROM node_embedding_refs r WHERE r.state='pending' AND EXISTS(SELECT 1 FROM nodes n WHERE n.node_id=r.node_id);"; }

start_daemon() {
  mkdir -p "$HOME_DIR"
  printf '[embedder]\n  rate_per_sec = 0\n' > "$HOME_DIR/config.toml"
  ln -sf "$BIN" "$DAEMON_LINK"
  VESKA_HOME="$HOME_DIR" VESKA_VECTOR_BACKEND=usearch "$DAEMON_LINK" >"$DAEMON_LOG" 2>&1 &
  DAEMON_PID=$!
  local waited=0
  until [ -S "$HOME_DIR/cli.sock" ]; do
    sleep 0.5
    kill -0 "$DAEMON_PID" 2>/dev/null || { tail -20 "$DAEMON_LOG" >&2; die "daemon exited at startup"; }
    waited=$((waited + 1)); [ "$waited" -gt $((READY_TIMEOUT * 2)) ] && die "daemon not ready"
  done
  sleep 1
}
stop_daemon() { [ -n "${DAEMON_PID:-}" ] && { kill "$DAEMON_PID" 2>/dev/null || true; wait "$DAEMON_PID" 2>/dev/null || true; DAEMON_PID=""; }; }
trap stop_daemon EXIT

drain_embeds() {
  local t0; t0=$(date +%s)
  while :; do
    [ "$(pending_embeds)" = "0" ] && { note "  embeds drained in $(( $(date +%s) - t0 ))s"; return; }
    [ $(( $(date +%s) - t0 )) -gt "$DRAIN_TIMEOUT" ] && { note "  embed drain TIMEOUT"; return; }
    sleep 3
  done
}

# --- build usearch-enabled binary ---
mkdir -p "$ROOT" "$CLONES"
note "building usearch binary (embed_model + hnsw_native)"
CGO_ENABLED=1 go -C "$REPO_SRC" build -tags "embed_model hnsw_native sqlite_fts5" -o "$BIN" ./cmd/veska \
  || die "build failed"

# --- fresh index? ---
if [ "$REINDEX" = "1" ] || [ ! -f "$DB" ]; then
  rm -rf "$HOME_DIR"
fi

start_daemon

# --- clone (cached) + index each repo ---
for entry in "${REPOS[@]}"; do
  read -r tier label src <<<"$entry"
  case "$src" in
    LOCAL:*) origin="${src#LOCAL:}" ;;
    *)       origin="https://github.com/$src" ;;
  esac
  clone="$CLONES/$label"
  if [ ! -d "$clone/.git" ]; then
    note "cloning $label ($origin)"
    git clone --depth 1 --quiet "$origin" "$clone" || die "clone $label failed"
  fi
  # already indexed? skip
  rid="$(q "SELECT repo_id FROM repos WHERE root_path='$clone';")"
  if [ -n "$rid" ] && [ "$rid" != "-1" ] && [ "$REINDEX" != "1" ]; then
    note "$label already indexed ($rid) - skipping"
    continue
  fi
  note "indexing $tier $label ..."
  VESKA_HOME="$HOME_DIR" "$BIN" repo add "$clone" --wait >/dev/null 2>&1 || die "repo add $label failed"
  drain_embeds
done

# --- build repo_id -> "tier:label" map for friendly table rows ---
labels=""
for entry in "${REPOS[@]}"; do
  read -r tier label src <<<"$entry"
  clone="$CLONES/$label"
  rid="$(q "SELECT repo_id FROM repos WHERE root_path='$clone' LIMIT 1;")"
  [ -n "$rid" ] && [ "$rid" != "-1" ] && labels="${labels:+$labels,}$rid=$tier:$label"
done
note "labels: $labels"

stop_daemon

# --- run the comparison harness against the indexed db ---
note "running TestBackendMatrix ..."
USEARCH_AB_DB="$DB" USEARCH_AB_LABELS="$labels" \
  CGO_ENABLED=1 go -C "$REPO_SRC" test -tags "eval hnsw_native sqlite_fts5" \
  -run TestBackendMatrix ./tools/loadtest/usearchab/ -v -count=1 -timeout 60m

note "done -> /tmp/backend-matrix.md"
