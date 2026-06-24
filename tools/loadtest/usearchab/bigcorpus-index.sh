#!/usr/bin/env bash
# bigcorpus-index.sh - index a slate of Go repos, EACH INTO ITS OWN ISOLATED
# HOME, to give the usearch build-profile calibration (eval-usearch-profile) a
# range of real corpus sizes to graph against. One repo per home/daemon/db keeps
# every data point PURE (no shared index/rehydrate state) and sidesteps the
# second-repo-add daemon panic (solov2-bihz). Mirrors backend-matrix.sh's proven
# daemon/clone/drain flow. Clones + indexes are cached; already-indexed repos
# are skipped, so this is safe to re-run.
#
#   ./tools/loadtest/usearchab/bigcorpus-index.sh
#   # then sweep EACH db separately and merge for the graph:
#   for d in /tmp/veska-corpus/*/home/veska.db; do
#     USEARCH_AB_DB=$d USEARCH_PROFILE_MAX_QUERIES=2000 USEARCH_PROFILE_MIN_NODES=2000 \
#       make eval-usearch-profile; done
set -euo pipefail

REPO_SRC="/home/jrose/src/engram/solov2"
BASE="/tmp/veska-corpus"
BIN="$BASE/veska"
READY_TIMEOUT=60
DRAIN_TIMEOUT=5400

# label | github owner/name. tidb is indexed separately (already done) in
# /tmp/veska-tidb/home; this slate fills the L/mid range. Ordered small->large.
REPOS=(
  "consul  hashicorp/consul"
  "vault   hashicorp/vault"
)

note() { printf '\033[1;36m[corpus]\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31m[corpus] FATAL:\033[0m %s\n' "$*" >&2; exit 1; }
q() { sqlite3 "$1" "$2" 2>/dev/null || echo -1; }
pending_embeds() { q "$1" "SELECT COUNT(*) FROM node_embedding_refs r WHERE r.state='pending' AND EXISTS(SELECT 1 FROM nodes n WHERE n.node_id=r.node_id);"; }

DAEMON_PID=""
HOME_DIR=""
start_daemon() {
  mkdir -p "$HOME_DIR"
  printf '[embedder]\n  rate_per_sec = 0\n' > "$HOME_DIR/config.toml"
  rm -f "$HOME_DIR/cli.sock"
  local link="$HOME_DIR/../veska-daemon"
  ln -sf "$BIN" "$link"
  VESKA_HOME="$HOME_DIR" VESKA_VECTOR_BACKEND=memory "$link" >"$HOME_DIR/../daemon.log" 2>&1 &
  DAEMON_PID=$!
  local waited=0
  until [ -S "$HOME_DIR/cli.sock" ]; do
    sleep 0.5
    kill -0 "$DAEMON_PID" 2>/dev/null || { tail -20 "$HOME_DIR/../daemon.log" >&2; die "daemon exited at startup"; }
    waited=$((waited + 1)); [ "$waited" -gt $((READY_TIMEOUT * 2)) ] && die "daemon not ready"
  done
  sleep 2 # let boot settle before repo add (solov2-bihz race margin)
}
stop_daemon() { [ -n "${DAEMON_PID:-}" ] && { kill "$DAEMON_PID" 2>/dev/null || true; wait "$DAEMON_PID" 2>/dev/null || true; DAEMON_PID=""; }; }
trap stop_daemon EXIT

drain_embeds() {
  local db="$1" t0; t0=$(date +%s); local last=-1 stuck=0 p
  while :; do
    p=$(pending_embeds "$db")
    [ "$p" = "0" ] && { note "  embeds drained in $(( $(date +%s) - t0 ))s"; return; }
    [ $(( $(date +%s) - t0 )) -gt "$DRAIN_TIMEOUT" ] && { note "  embed drain TIMEOUT at pending=$p"; return; }
    if [ "$p" = "$last" ]; then stuck=$((stuck+1)); else stuck=0; fi
    [ $(( stuck % 12 )) = 0 ] && note "  draining... pending=$p ($(( $(date +%s) - t0 ))s)"
    last=$p; sleep 5
  done
}

mkdir -p "$BASE"
note "building usearch+embed_model binary"
CGO_ENABLED=1 go -C "$REPO_SRC" build -tags "embed_model hnsw_native sqlite_fts5" -o "$BIN" ./cmd/veska || die "build failed"

for entry in "${REPOS[@]}"; do
  label="${entry%% *}"; src="${entry##* }"
  ROOT="$BASE/$label"; HOME_DIR="$ROOT/home"; DB="$HOME_DIR/veska.db"; CLONE="$ROOT/repo"
  mkdir -p "$ROOT"
  nodes_done="$(q "$DB" "SELECT COUNT(*) FROM nodes;")"
  if [ "$nodes_done" != "-1" ] && [ "$nodes_done" -gt 0 ] 2>/dev/null; then
    note "$label already indexed ($nodes_done nodes) at $DB - skipping"
    continue
  fi
  [ -d "$CLONE/.git" ] || { note "cloning $label ($src)..."; git clone --depth 1 --quiet "https://github.com/$src" "$CLONE" || die "clone $label failed"; }
  start_daemon
  note "indexing $label (cold scan + promote)..."
  VESKA_HOME="$HOME_DIR" "$BIN" repo add "$CLONE" --wait >/dev/null 2>&1 || { tail -20 "$ROOT/daemon.log" >&2; die "repo add $label failed"; }
  drain_embeds "$DB"
  note "$label DONE: $(q "$DB" "SELECT COUNT(*) FROM nodes;") nodes  db=$DB"
  stop_daemon
done

note "ALL DONE. corpus dbs (one repo each):"
for entry in "${REPOS[@]}"; do
  label="${entry%% *}"; db="$BASE/$label/home/veska.db"
  note "  $label: $(q "$db" "SELECT COUNT(*) FROM nodes;") nodes  $db"
done
note "  tidb: $(q /tmp/veska-tidb/home/veska.db "SELECT COUNT(*) FROM nodes;") nodes  /tmp/veska-tidb/home/veska.db"
