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
  "S   go-git  go-git/go-git"
  "M   veska   LOCAL:$REPO_SRC"
  "M2  grpc-go grpc/grpc-go"
  "L   consul  hashicorp/consul"
  "L2  vault   hashicorp/vault"
  "XL  tidb    pingcap/tidb"
  # Heavy XXL repo - uncomment to extend the curve further. Clones GBs and takes
  # tens of minutes to index; set the sampling knobs below when you do.
  # "XXL k8s     kubernetes/kubernetes"
)

# For big repos: USEARCH_AB_MAX_QUERIES samples the per-node autolink sweep
# (memvec is O(n^2), intractable past ~50k nodes); USEARCH_AB_MEMVEC_MAX_NODES
# skips the in-RAM memvec build above that node count (usearch-only row). Both
# default off. Suggested for the big repos: 2000 and 150000.
export USEARCH_AB_MAX_QUERIES=${USEARCH_AB_MAX_QUERIES:-0}
export USEARCH_AB_MEMVEC_MAX_NODES=${USEARCH_AB_MEMVEC_MAX_NODES:-0}

note() { printf '\033[1;36m[matrix]\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31m[matrix] FATAL:\033[0m %s\n' "$*" >&2; exit 1; }

REINDEX=0
[ "${1:-}" = "--reindex" ] && REINDEX=1

q() { sqlite3 "$DB" "$1" 2>/dev/null || echo -1; }
pending_embeds() { q "SELECT COUNT(*) FROM node_embedding_refs r WHERE r.state='pending' AND EXISTS(SELECT 1 FROM nodes n WHERE n.node_id=r.node_id);"; }

TIMES="$HOME_DIR/index-times.tsv"  # repo_id<TAB>seconds (parse+embed); persists across runs

start_daemon() {
  mkdir -p "$HOME_DIR"
  printf '[embedder]\n  rate_per_sec = 0\n' > "$HOME_DIR/config.toml"
  rm -f "$HOME_DIR/cli.sock"
  ln -sf "$BIN" "$DAEMON_LINK"
  # Index with the memory backend: node_embeddings are backend-independent (the
  # harness builds and compares BOTH backends from the stored vectors later), and
  # memvec's trivial upsert keeps the embed lane fast - the usearch HNSW upsert
  # during cold-scan slows it and aggravates the embed-poller stall (solov2-b5aw).
  VESKA_HOME="$HOME_DIR" VESKA_VECTOR_BACKEND=memory "$DAEMON_LINK" >"$DAEMON_LOG" 2>&1 &
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
  local t0; t0=$(date +%s); local last=-1 stuck=0 p restarts=0
  while :; do
    p=$(pending_embeds)
    [ "$p" = "0" ] && { note "  embeds drained in $(( $(date +%s) - t0 ))s"; return; }
    [ $(( $(date +%s) - t0 )) -gt "$DRAIN_TIMEOUT" ] && { note "  embed drain TIMEOUT at pending=$p"; return; }
    if [ "$p" = "$last" ]; then stuck=$((stuck+1)); else stuck=0; fi
    # ~90s with NO progress = embed poller genuinely stalled (solov2-b5aw); a
    # restart resumes it. Cap restarts and grace the post-restart rehydrate
    # (which itself looks like a stall) so we don't death-spiral.
    if [ "$stuck" -ge 18 ] && [ "$restarts" -lt 4 ]; then
      restarts=$((restarts + 1))
      note "  embed poller stalled at pending=$p - restart $restarts (solov2-b5aw)"
      stop_daemon; start_daemon
      sleep 30  # let rehydrate finish before re-arming the stall detector
      stuck=0; last=-1
    fi
    last=$p; sleep 5
  done
}

# record_time <repo_id> <seconds> - update-in-place so cached repos keep their time
record_time() {
  grep -v "^$1	" "$TIMES" 2>/dev/null > "$TIMES.tmp" || true
  mv -f "$TIMES.tmp" "$TIMES" 2>/dev/null || true
  printf '%s\t%s\n' "$1" "$2" >> "$TIMES"
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
  ti=$(date +%s)
  VESKA_HOME="$HOME_DIR" "$BIN" repo add "$clone" --wait >/dev/null 2>&1 || die "repo add $label failed"
  drain_embeds
  idx=$(( $(date +%s) - ti ))
  rid_now="$(q "SELECT repo_id FROM repos WHERE root_path='$clone';")"
  [ -n "$rid_now" ] && [ "$rid_now" != "-1" ] && record_time "$rid_now" "$idx"
  note "  $label indexed (parse+embed) in ${idx}s"
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

# --- assemble repo_id=seconds index-time map (parse+embed) for the table ---
itimes=""
if [ -f "$TIMES" ]; then
  while IFS=$'\t' read -r rid secs; do
    [ -n "$rid" ] && itimes="${itimes:+$itimes,}$rid=$secs"
  done < "$TIMES"
fi
note "index times: $itimes"

# --- run the comparison harness against the indexed db ---
note "running TestBackendMatrix ..."
USEARCH_AB_DB="$DB" USEARCH_AB_LABELS="$labels" USEARCH_AB_INDEX_TIMES="$itimes" \
  CGO_ENABLED=1 go -C "$REPO_SRC" test -tags "eval hnsw_native sqlite_fts5" \
  -run TestBackendMatrix ./tools/loadtest/usearchab/ -v -count=1 -timeout 60m

note "done -> /tmp/backend-matrix.md"
