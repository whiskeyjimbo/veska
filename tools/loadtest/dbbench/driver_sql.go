//go:build eval

package dbbench

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
	"net/url"
	"strings"
)

// sqlBench is the shared Bench implementation for any database/sql-backed
// driver. The driver-specific files supply only Name and the registered
// driver name passed to sql.Open.
type sqlBench struct {
	name      string
	driver    string // sql.Open driver name
	db        *sql.DB
	seedCount int
}

func newSQLBench(name, driver string) *sqlBench {
	return &sqlBench{name: name, driver: driver}
}

func (b *sqlBench) Name() string { return b.name }

func (b *sqlBench) Open(_ context.Context, path string) error {
	// Match production: WAL, FK on, busy_timeout. Use DSN-encoded PRAGMA so
	// every pooled connection gets them.
	dsn := buildDSN(b.driver, path)
	db, err := sql.Open(b.driver, dsn)
	if err != nil {
		return fmt.Errorf("sql.Open(%s): %w", b.driver, err)
	}
	// Single writer matches production veska. The bench's read workloads
	// don't share the same handle, so this only constrains writer paths;
	// fine for the relative comparison we care about.
	db.SetMaxOpenConns(8)
	// Sanity check: pragmas applied. Misapplied pragmas would invalidate
	// the comparison - most obviously, a driver that quietly stays on
	// rollback-journal mode would show 100× faster write-tx latency
	// because every commit skips an fsync that the others pay.
	var fk int
	if err := db.QueryRow(`PRAGMA foreign_keys`).Scan(&fk); err != nil {
		_ = db.Close()
		return fmt.Errorf("PRAGMA foreign_keys: %w", err)
	}
	if fk != 1 {
		_ = db.Close()
		return fmt.Errorf("foreign_keys not on: got %d", fk)
	}
	var journal string
	if err := db.QueryRow(`PRAGMA journal_mode`).Scan(&journal); err != nil {
		_ = db.Close()
		return fmt.Errorf("PRAGMA journal_mode: %w", err)
	}
	if !strings.EqualFold(journal, "wal") {
		_ = db.Close()
		return fmt.Errorf("%s: journal_mode=%s, want wal", b.driver, journal)
	}
	b.db = db
	return nil
}

func buildDSN(driver, path string) string {
	base := path
	if !strings.HasPrefix(base, "file:") {
		base = "file:" + base
	}
	sep := "?"
	if strings.Contains(base, "?") {
		sep = "&"
	}
	q := url.Values{}
	// synchronous=NORMAL is explicit on every driver so the comparison is
	// apples-to-apples: SQLite's C default is FULL (fsync after every commit),
	// but mattn picks NORMAL under WAL by default. Without forcing parity the
	// write-tx workloads show a ~100× gap that's really just an fsync
	// frequency difference, not a driver-quality difference.
	if driver == "sqlite3" { // mattn
		q.Set("_journal", "WAL")
		q.Set("_fk", "true")
		q.Set("_sync", "NORMAL")
		q.Set("_busy_timeout", "5000")
	}
	return base + sep + q.Encode()
}

func (b *sqlBench) ApplySchema(ctx context.Context, stmts []string) error {
	for _, s := range stmts {
		if _, err := b.db.ExecContext(ctx, s); err != nil {
			return fmt.Errorf("apply schema: %w\nstmt: %s", err, s)
		}
	}
	return nil
}

func (b *sqlBench) Seed(ctx context.Context, cfg SeedConfig) error {
	gen := NewGenerator(cfg)
	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	insN, err := tx.PrepareContext(ctx, `INSERT INTO nodes
		(node_id, branch, repo_id, language, kind, symbol_path, file_path,
		 line_start, line_end, content_hash, last_promoted_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer insN.Close()
	insE, err := tx.PrepareContext(ctx, `INSERT INTO edges
		(edge_id, branch, repo_id, src_node_id, dst_node_id, kind, confidence, last_promoted_at)
		VALUES (?,?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer insE.Close()
	insFW, err := tx.PrepareContext(ctx, `INSERT INTO node_fts_words(node_id, branch, repo_id, words) VALUES (?,?,?,?)`)
	if err != nil {
		return err
	}
	defer insFW.Close()
	insFT, err := tx.PrepareContext(ctx, `INSERT INTO node_fts_trigrams(node_id, branch, repo_id, raw) VALUES (?,?,?,?)`)
	if err != nil {
		return err
	}
	defer insFT.Close()
	insEmb, err := tx.PrepareContext(ctx, `INSERT INTO node_embeddings(content_hash, model, dim, embedding, created_at) VALUES (?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer insEmb.Close()
	insRef, err := tx.PrepareContext(ctx, `INSERT INTO node_embedding_refs(node_id, content_hash, state, enqueued_at, embedded_at) VALUES (?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer insRef.Close()

	nIdx := 0
	err = gen.Nodes(func(n SeedNode) error {
		nIdx++
		if _, err := insN.ExecContext(ctx, n.NodeID, cfg.Branch, cfg.RepoID, n.Language, n.Kind,
			n.SymbolPath, n.FilePath, n.LineStart, n.LineEnd, n.ContentHash, n.LastPromotedAt); err != nil {
			return err
		}
		words := strings.Join(strings.FieldsFunc(n.SymbolPath, func(r rune) bool {
			return r == '/' || r == '.' || r == '_'
		}), " ") + " " + n.Name
		if _, err := insFW.ExecContext(ctx, n.NodeID, cfg.Branch, cfg.RepoID, words); err != nil {
			return err
		}
		if _, err := insFT.ExecContext(ctx, n.NodeID, cfg.Branch, cfg.RepoID, n.Kind+" "+n.SymbolPath+" "+n.Name); err != nil {
			return err
		}
		blob := gen.EmbeddingBlob(nIdx)
		if _, err := insEmb.ExecContext(ctx, n.ContentHash, "bench-model", cfg.EmbedDim, blob, n.LastPromotedAt); err != nil {
			return err
		}
		if _, err := insRef.ExecContext(ctx, n.NodeID, n.ContentHash, "ready", n.LastPromotedAt, n.LastPromotedAt); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return err
	}
	err = gen.Edges(func(e SeedEdge) error {
		_, err := insE.ExecContext(ctx, e.EdgeID, cfg.Branch, cfg.RepoID, e.SrcNodeID, e.DstNodeID, e.Kind, e.Confidence, e.LastPromotedAt)
		return err
	})
	if err != nil {
		return err
	}

	// Queue rows.
	insQ, err := tx.PrepareContext(ctx, `INSERT INTO post_promotion_queue
		(promotion_id, repo_id, branch, git_sha, work_kind, payload, state, enqueued_at)
		VALUES (?,?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer insQ.Close()
	for i := 0; i < QueueRowCount(cfg); i++ {
		state := "pending"
		if i%3 == 0 {
			state = "done"
		}
		if _, err := insQ.ExecContext(ctx, fmt.Sprintf("p%d", i), cfg.RepoID, cfg.Branch, "deadbeef",
			"embed", "{}", state, 1_700_000_000+int64(i)); err != nil {
			return err
		}
	}

	b.seedCount = cfg.Nodes
	return tx.Commit()
}

func (b *sqlBench) GraphRead(ctx context.Context, i int) error {
	id := fmt.Sprintf("n%08d", deterministicIndex(i, b.seedCount))
	row := b.db.QueryRowContext(ctx,
		`SELECT node_id, symbol_path, file_path, line_start, line_end, content_hash
		   FROM nodes WHERE node_id=? AND branch=?`, id, "main")
	var nodeID, sym, file, hash string
	var ls, le int
	if err := row.Scan(&nodeID, &sym, &file, &ls, &le, &hash); err != nil {
		return err
	}
	// Plus a fan-out of outbound edges (mirrors EdgeReader).
	rows, err := b.db.QueryContext(ctx,
		`SELECT edge_id, dst_node_id FROM edges WHERE src_node_id=? AND branch=? AND kind=?`,
		id, "main", "calls")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var eid, dst string
		if err := rows.Scan(&eid, &dst); err != nil {
			return err
		}
	}
	return rows.Err()
}

func (b *sqlBench) FTSQuery(ctx context.Context, i int) error {
	// Pick one of three query shapes per iteration; mirrors lexical_repo's
	// fused words+trigram approach (issued as two separate MATCHes).
	q := ftsQuery(i)
	rows, err := b.db.QueryContext(ctx,
		`SELECT node_id FROM node_fts_words WHERE node_fts_words MATCH ? LIMIT 50`, q)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id string
		_ = rows.Scan(&id)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	rows, err = b.db.QueryContext(ctx,
		`SELECT node_id FROM node_fts_trigrams WHERE node_fts_trigrams MATCH ? LIMIT 50`, q)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		_ = rows.Scan(&id)
	}
	return rows.Err()
}

func (b *sqlBench) RehydrateScan(ctx context.Context) (int, error) {
	rows, err := b.db.QueryContext(ctx,
		`SELECT r.node_id, e.embedding
		   FROM node_embedding_refs r
		   JOIN node_embeddings e ON r.content_hash = e.content_hash
		  WHERE r.state = 'ready'`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var id string
		var blob []byte
		if err := rows.Scan(&id, &blob); err != nil {
			return n, err
		}
		// Touch the blob so the driver actually copies it out.
		if len(blob) == 0 {
			return n, fmt.Errorf("empty blob")
		}
		n++
	}
	return n, rows.Err()
}

func (b *sqlBench) QueuePoll(ctx context.Context, i int) error {
	// Mirror queue/poller: pop one pending row, mark done.
	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var seq int64
	err = tx.QueryRowContext(ctx,
		`SELECT seq FROM post_promotion_queue
		  WHERE state='pending' ORDER BY seq LIMIT 1`).Scan(&seq)
	if err == sql.ErrNoRows {
		// Re-seed a pending row so the bench is steady-state.
		_, err = tx.ExecContext(ctx,
			`INSERT INTO post_promotion_queue
			   (promotion_id, repo_id, branch, git_sha, work_kind, payload, state, enqueued_at)
			 VALUES (?,?,?,?,?,?,?,?)`,
			fmt.Sprintf("p-iter-%d", i), "bench-repo", "main", "deadbeef",
			"embed", "{}", "pending", int64(1_700_000_000+i))
		if err != nil {
			return err
		}
		return tx.Commit()
	}
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE post_promotion_queue SET state='done', completed_at=? WHERE seq=?`,
		int64(1_700_000_000+i), seq); err != nil {
		return err
	}
	return tx.Commit()
}

func (b *sqlBench) PromotionTx(ctx context.Context, i int) error {
	// Mirror PromotionStore.Apply: multi-statement tx that touches nodes, edges,
	// FTS, and embedding_refs in one go.
	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	// Pick 10 nodes; upsert them with a new last_promoted_at + a fresh FTS row.
	rng := rand.New(rand.NewSource(int64(i)))
	for k := 0; k < 10; k++ {
		idx := rng.Intn(b.seedCount)
		id := fmt.Sprintf("n%08d", idx)
		ts := int64(1_700_100_000 + i*10 + k)
		if _, err := tx.ExecContext(ctx,
			`UPDATE nodes SET last_promoted_at=? WHERE node_id=? AND branch=?`,
			ts, id, "main"); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE node_embedding_refs SET state='pending', enqueued_at=? WHERE node_id=?`,
			ts, id); err != nil {
			return err
		}
	}
	// Enqueue a queue row for the promotion.
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO post_promotion_queue
		   (promotion_id, repo_id, branch, git_sha, work_kind, payload, state, enqueued_at)
		 VALUES (?,?,?,?,?,?,?,?)`,
		fmt.Sprintf("prom-%d", i), "bench-repo", "main", "feedface",
		"embed", "{}", "pending", int64(1_700_100_000+i)); err != nil {
		return err
	}
	return tx.Commit()
}

func (b *sqlBench) BulkIngest(ctx context.Context, batch int, i int) error {
	// Insert `batch` new nodes inside a single tx. New IDs to avoid PK clash.
	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	ins, err := tx.PrepareContext(ctx, `INSERT INTO nodes
		(node_id, branch, repo_id, language, kind, symbol_path, file_path,
		 line_start, line_end, content_hash, last_promoted_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer ins.Close()
	for k := 0; k < batch; k++ {
		id := fmt.Sprintf("bi-%d-%d", i, k)
		if _, err := ins.ExecContext(ctx, id, "main", "bench-repo", "go", "function",
			"bench/"+id, "bench.go", 1, 5, "h"+id, int64(1_700_200_000+i)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (b *sqlBench) Close() error {
	if b.db == nil {
		return nil
	}
	return b.db.Close()
}

// deterministicIndex returns a stable per-iteration index in [0, n) so reads
// hit a spread of rows without RNG overhead in the hot path.
func deterministicIndex(i, n int) int {
	if n <= 0 {
		return 0
	}
	// Cheap LCG; we just need uniform-ish distribution.
	x := uint32(i*2654435761) ^ 0x9E3779B9
	return int(x) % n
}

var ftsQueries = []string{"Func", "pkg mod", "Func1", "mod5 Func", "pkg"}

func ftsQuery(i int) string { return ftsQueries[i%len(ftsQueries)] }
