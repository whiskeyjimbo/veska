// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

// Package pkloader provides a synthetic data loader for the branch-in-PK SQLite schema
// defined in /§3.2.
package pkloader

import (
	"bufio"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// LoadMetrics records the measured results for a single load run.
type LoadMetrics struct {
	OverlapPct   int   `json:"overlap_pct"`
	Branches     int   `json:"branches"`
	Symbols      int   `json:"symbols"`
	NodeRows     int64 `json:"node_rows"`
	EdgeRows     int64 `json:"edge_rows"`
	FindingRows  int64 `json:"finding_rows"`
	DBBytes      int64 `json:"db_bytes"`
	WALBytes     int64 `json:"wal_bytes"`
	PeakRSSBytes int64 `json:"peak_rss_bytes"`
	LoadWallMs   int64 `json:"load_wall_ms"`
}

// Symbol is a single code symbol (node) with its stable identity and content hash.
type Symbol struct {
	NodeID      string
	SymbolPath  string
	ContentHash string
}

// CreateSchema creates all tables and indexes on db (verbatim from /§3.2).
func CreateSchema(db *sql.DB) error {
	_, err := db.Exec(`
CREATE TABLE IF NOT EXISTS repos (
    repo_id          TEXT PRIMARY KEY,
    root_path        TEXT NOT NULL UNIQUE,
    added_at         INTEGER NOT NULL,
    active_branch    TEXT,
    last_promoted_sha TEXT,
    module_path      TEXT
);

CREATE TABLE IF NOT EXISTS nodes (
    node_id        TEXT NOT NULL,
    branch         TEXT NOT NULL,
    repo_id        TEXT NOT NULL,
    language       TEXT NOT NULL,
    kind           TEXT NOT NULL,
    symbol_path    TEXT NOT NULL,
    file_path      TEXT NOT NULL,
    line_start     INTEGER,
    line_end       INTEGER,
    content_hash   TEXT NOT NULL,
    last_promoted_at INTEGER NOT NULL,
    actor_id       TEXT NOT NULL,
    actor_kind     TEXT NOT NULL CHECK (actor_kind IN ('human','agent','system')),
    PRIMARY KEY (node_id, branch),
    FOREIGN KEY (repo_id) REFERENCES repos(repo_id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_nodes_repo_branch ON nodes(repo_id, branch);
CREATE INDEX IF NOT EXISTS idx_nodes_symbol ON nodes(symbol_path);
CREATE INDEX IF NOT EXISTS idx_nodes_content_hash ON nodes(content_hash);

CREATE TABLE IF NOT EXISTS edges (
    edge_id        TEXT NOT NULL,
    branch         TEXT NOT NULL,
    repo_id        TEXT NOT NULL,
    src_node_id    TEXT NOT NULL,
    dst_node_id    TEXT NOT NULL,
    kind           TEXT NOT NULL,
    confidence     TEXT NOT NULL,
    last_promoted_at INTEGER NOT NULL,
    PRIMARY KEY (edge_id, branch),
    FOREIGN KEY (src_node_id, branch) REFERENCES nodes(node_id, branch) ON DELETE CASCADE,
    FOREIGN KEY (dst_node_id, branch) REFERENCES nodes(node_id, branch) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_edges_src ON edges(src_node_id, branch, kind);
CREATE INDEX IF NOT EXISTS idx_edges_dst ON edges(dst_node_id, branch, kind);
CREATE INDEX IF NOT EXISTS idx_edges_repo_branch ON edges(repo_id, branch);

CREATE TABLE IF NOT EXISTS findings (
    finding_id    TEXT NOT NULL,
    branch        TEXT NOT NULL,
    repo_id       TEXT NOT NULL,
    node_id       TEXT,
    file_path     TEXT,
    severity      TEXT NOT NULL,
    source_layer  TEXT NOT NULL,
    rule          TEXT NOT NULL,
    message       TEXT NOT NULL,
    state         TEXT NOT NULL,
    closed_reason TEXT,
    created_at    INTEGER NOT NULL,
    closed_at     INTEGER,
    actor_id      TEXT NOT NULL,
    actor_kind    TEXT NOT NULL CHECK (actor_kind IN ('human','agent','system')),
    PRIMARY KEY (finding_id, branch),
    FOREIGN KEY (repo_id) REFERENCES repos(repo_id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_findings_state ON findings(state, severity);
CREATE INDEX IF NOT EXISTS idx_findings_anchor ON findings(node_id, branch);
CREATE INDEX IF NOT EXISTS idx_findings_repo_branch ON findings(repo_id, branch);
`)
	return err
}

// InsertRepo inserts a seed repo row; idempotent via INSERT OR IGNORE.
func InsertRepo(db *sql.DB, repoID string) error {
	_, err := db.Exec(
		`INSERT OR IGNORE INTO repos (repo_id, root_path, added_at, active_branch) VALUES (?, ?, ?, ?)`,
		repoID, "/"+repoID, 1700000000, "main",
	)
	return err
}

// hashStr returns a short hex digest for a string.
func hashStr(s string) string {
	h := sha256.Sum256([]byte(s))
	return fmt.Sprintf("%x", h[:8])
}

// GenerateBaseSymbols generates n stable, deterministic symbols for repoID.
// Symbol IDs are deterministic hashes of (repoID, i).
func GenerateBaseSymbols(n int, repoID string) []Symbol {
	syms := make([]Symbol, n)
	for i := range syms {
		key := fmt.Sprintf("%s:%d", repoID, i)
		nodeID := "n-" + hashStr(key)
		contentHash := "h-" + hashStr(key+":content")
		syms[i] = Symbol{
			NodeID:      nodeID,
			SymbolPath:  fmt.Sprintf("%s/pkg/sym_%d.Func", repoID, i),
			ContentHash: contentHash,
		}
	}
	return syms
}

// xorshift64 is a simple deterministic PRNG.
func xorshift64(state *uint64) uint64 {
	x := *state
	x ^= x << 13
	x ^= x >> 7
	x ^= x << 17
	*state = x
	return x
}

// ApplyDirtyOverlap returns a copy of symbols where overlapPct% have a new content_hash.
// Uses seed for determinism. The returned slice has the same length as symbols.
func ApplyDirtyOverlap(symbols []Symbol, overlapPct int, seed uint64) []Symbol {
	result := make([]Symbol, len(symbols))
	copy(result, symbols)

	n := len(symbols)
	dirtyCount := (n * overlapPct) / 100

	// Build a deterministic shuffled index list using Fisher-Yates with our PRNG.
	indices := make([]int, n)
	for i := range indices {
		indices[i] = i
	}
	state := seed
	for i := n - 1; i > 0; i-- {
		j := int(xorshift64(&state) % uint64(i+1))
		indices[i], indices[j] = indices[j], indices[i]
	}

	// Mark the first dirtyCount as dirty.
	for k := range dirtyCount {
		idx := indices[k]
		result[idx].ContentHash = "dirty-" + hashStr(fmt.Sprintf("%s:%d:%d", result[idx].NodeID, seed, k))
	}
	return result
}

// InsertBranch inserts all symbols+edges+findings for one branch in a single transaction.
// Findings: 1 per 100 symbols (integer division).
func InsertBranch(db *sql.DB, branch, repoID string, symbols []Symbol, ts int64) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	// Prepare node insert.
	nodeStmt, err := tx.Prepare(`
		INSERT INTO nodes
		  (node_id, branch, repo_id, language, kind, symbol_path, file_path,
		   line_start, line_end, content_hash, last_promoted_at, actor_id, actor_kind)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return fmt.Errorf("prepare node stmt: %w", err)
	}
	defer nodeStmt.Close()

	// Prepare edge insert.
	edgeStmt, err := tx.Prepare(`
		INSERT INTO edges
		  (edge_id, branch, repo_id, src_node_id, dst_node_id, kind, confidence, last_promoted_at)
		VALUES (?,?,?,?,?,?,?,?)`)
	if err != nil {
		return fmt.Errorf("prepare edge stmt: %w", err)
	}
	defer edgeStmt.Close()

	// Prepare finding insert.
	findingStmt, err := tx.Prepare(`
		INSERT INTO findings
		  (finding_id, branch, repo_id, node_id, file_path, severity, source_layer,
		   rule, message, state, closed_reason, created_at, closed_at, actor_id, actor_kind)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return fmt.Errorf("prepare finding stmt: %w", err)
	}
	defer findingStmt.Close()

	n := len(symbols)
	for i, sym := range symbols {
		filePath := fmt.Sprintf("src/%s.go", sym.SymbolPath)
		_, err := nodeStmt.Exec(
			sym.NodeID, branch, repoID,
			"go", "func", sym.SymbolPath, filePath,
			i*10+1, i*10+5,
			sym.ContentHash, ts, "system", "system",
		)
		if err != nil {
			return fmt.Errorf("insert node[%d]: %w", i, err)
		}

		// 1 CALLS edge per symbol, connecting to next symbol (circular).
		nextSym := symbols[(i+1)%n]
		edgeID := "e-" + hashStr(fmt.Sprintf("%s->%s:%s", sym.NodeID, nextSym.NodeID, branch))
		_, err = edgeStmt.Exec(
			edgeID, branch, repoID,
			sym.NodeID, nextSym.NodeID,
			"CALLS", "1.0", ts,
		)
		if err != nil {
			return fmt.Errorf("insert edge[%d]: %w", i, err)
		}

		// 1 finding per 100 symbols.
		if i%100 == 0 {
			findingID := "f-" + hashStr(fmt.Sprintf("%s:%s:%d", branch, repoID, i))
			_, err = findingStmt.Exec(
				findingID, branch, repoID,
				sym.NodeID, filePath,
				"medium", "static", "lint/unused",
				fmt.Sprintf("unused symbol at index %d", i),
				"open", nil, ts, nil,
				"system", "system",
			)
			if err != nil {
				return fmt.Errorf("insert finding[%d]: %w", i, err)
			}
		}
	}

	return tx.Commit()
}

// ReadRSSBytes reads VmRSS from /proc/self/status (Linux); returns 0 elsewhere.
func ReadRSSBytes() int64 {
	f, err := os.Open("/proc/self/status")
	if err != nil {
		return 0
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "VmRSS:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				kb, err := strconv.ParseInt(fields[1], 10, 64)
				if err == nil {
					return kb * 1024
				}
			}
		}
	}
	return 0
}
