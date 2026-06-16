//go:build eval

package dbbench

import (
	"context"
	"fmt"
	"math/rand"
	"strings"

	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

func init() {
	Register("zombiezen", func() Bench { return &zbBench{} })
}

// zbBench is a parallel Bench implementation using zombiezen's non-database/sql
// API. The query strings match driver_sql.go's; only the bind/scan harness
// differs. This faithfulness is the point of including zombiezen at all
type zbBench struct {
	pool      *sqlitex.Pool
	seedCount int
}

func (b *zbBench) Name() string { return "zombiezen" }

func (b *zbBench) Open(_ context.Context, path string) error {
	uri := "file:" + path + "?_journal_mode=WAL&_foreign_keys=on&_busy_timeout=5000"
	pool, err := sqlitex.NewPool(uri, sqlitex.PoolOptions{
		PoolSize: 8,
		PrepareConn: func(conn *sqlite.Conn) error {
			// Mirror driver_sql.go: WAL is per-db, FK + busy_timeout per-conn.
			if err := sqlitex.ExecuteTransient(conn, "PRAGMA journal_mode=WAL;", nil); err != nil {
				return err
			}
			if err := sqlitex.ExecuteTransient(conn, "PRAGMA foreign_keys=on;", nil); err != nil {
				return err
			}
			// Match driver_sql.go — see comment there for why synchronous=NORMAL
			// is set explicitly across drivers.
			if err := sqlitex.ExecuteTransient(conn, "PRAGMA synchronous=NORMAL;", nil); err != nil {
				return err
			}
			return sqlitex.ExecuteTransient(conn, "PRAGMA busy_timeout=5000;", nil)
		},
	})
	if err != nil {
		return fmt.Errorf("zombiezen NewPool: %w", err)
	}
	b.pool = pool
	return nil
}

func (b *zbBench) conn(ctx context.Context) (*sqlite.Conn, func(), error) {
	c, err := b.pool.Take(ctx)
	if err != nil {
		return nil, nil, err
	}
	return c, func() { b.pool.Put(c) }, nil
}

func (b *zbBench) ApplySchema(ctx context.Context, stmts []string) error {
	conn, done, err := b.conn(ctx)
	if err != nil {
		return err
	}
	defer done()
	for _, s := range stmts {
		if err := sqlitex.ExecuteScript(conn, s, nil); err != nil {
			return fmt.Errorf("apply schema: %w\nstmt: %s", err, s)
		}
	}
	return nil
}

func (b *zbBench) Seed(ctx context.Context, cfg SeedConfig) error {
	conn, done, err := b.conn(ctx)
	if err != nil {
		return err
	}
	defer done()

	endFn, err := sqlitex.ImmediateTransaction(conn)
	if err != nil {
		return err
	}
	var commitErr error
	defer func() { endFn(&commitErr) }()

	gen := NewGenerator(cfg)
	idx := 0
	err = gen.Nodes(func(n SeedNode) error {
		idx++
		if err := sqlitex.Execute(conn, `INSERT INTO nodes
			(node_id, branch, repo_id, language, kind, symbol_path, file_path,
			 line_start, line_end, content_hash, last_promoted_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?)`, &sqlitex.ExecOptions{
			Args: []any{n.NodeID, cfg.Branch, cfg.RepoID, n.Language, n.Kind, n.SymbolPath,
				n.FilePath, n.LineStart, n.LineEnd, n.ContentHash, n.LastPromotedAt},
		}); err != nil {
			return err
		}
		words := strings.Join(strings.FieldsFunc(n.SymbolPath, func(r rune) bool {
			return r == '/' || r == '.' || r == '_'
		}), " ") + " " + n.Name
		if err := sqlitex.Execute(conn,
			`INSERT INTO node_fts_words(node_id, branch, repo_id, words) VALUES (?,?,?,?)`,
			&sqlitex.ExecOptions{Args: []any{n.NodeID, cfg.Branch, cfg.RepoID, words}}); err != nil {
			return err
		}
		if err := sqlitex.Execute(conn,
			`INSERT INTO node_fts_trigrams(node_id, branch, repo_id, raw) VALUES (?,?,?,?)`,
			&sqlitex.ExecOptions{Args: []any{n.NodeID, cfg.Branch, cfg.RepoID, n.Kind + " " + n.SymbolPath + " " + n.Name}}); err != nil {
			return err
		}
		blob := gen.EmbeddingBlob(idx)
		if err := sqlitex.Execute(conn,
			`INSERT INTO node_embeddings(content_hash, model, dim, embedding, created_at) VALUES (?,?,?,?,?)`,
			&sqlitex.ExecOptions{Args: []any{n.ContentHash, "bench-model", cfg.EmbedDim, blob, n.LastPromotedAt}}); err != nil {
			return err
		}
		return sqlitex.Execute(conn,
			`INSERT INTO node_embedding_refs(node_id, content_hash, state, enqueued_at, embedded_at) VALUES (?,?,?,?,?)`,
			&sqlitex.ExecOptions{Args: []any{n.NodeID, n.ContentHash, "ready", n.LastPromotedAt, n.LastPromotedAt}})
	})
	if err != nil {
		commitErr = err
		return err
	}
	err = gen.Edges(func(e SeedEdge) error {
		return sqlitex.Execute(conn, `INSERT INTO edges
			(edge_id, branch, repo_id, src_node_id, dst_node_id, kind, confidence, last_promoted_at)
			VALUES (?,?,?,?,?,?,?,?)`, &sqlitex.ExecOptions{
			Args: []any{e.EdgeID, cfg.Branch, cfg.RepoID, e.SrcNodeID, e.DstNodeID, e.Kind, e.Confidence, e.LastPromotedAt},
		})
	})
	if err != nil {
		commitErr = err
		return err
	}
	for i := 0; i < QueueRowCount(cfg); i++ {
		state := "pending"
		if i%3 == 0 {
			state = "done"
		}
		if err := sqlitex.Execute(conn, `INSERT INTO post_promotion_queue
			(promotion_id, repo_id, branch, git_sha, work_kind, payload, state, enqueued_at)
			VALUES (?,?,?,?,?,?,?,?)`, &sqlitex.ExecOptions{
			Args: []any{fmt.Sprintf("p%d", i), cfg.RepoID, cfg.Branch, "deadbeef",
				"embed", "{}", state, int64(1_700_000_000 + i)},
		}); err != nil {
			commitErr = err
			return err
		}
	}
	b.seedCount = cfg.Nodes
	return nil
}

func (b *zbBench) GraphRead(ctx context.Context, i int) error {
	conn, done, err := b.conn(ctx)
	if err != nil {
		return err
	}
	defer done()
	id := fmt.Sprintf("n%08d", deterministicIndex(i, b.seedCount))
	found := false
	if err := sqlitex.Execute(conn,
		`SELECT node_id, symbol_path, file_path, line_start, line_end, content_hash
		   FROM nodes WHERE node_id=? AND branch=?`, &sqlitex.ExecOptions{
			Args: []any{id, "main"},
			ResultFunc: func(stmt *sqlite.Stmt) error {
				found = true
				_ = stmt.ColumnText(0)
				_ = stmt.ColumnText(1)
				return nil
			},
		}); err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("graph_read miss: %s", id)
	}
	return sqlitex.Execute(conn,
		`SELECT edge_id, dst_node_id FROM edges WHERE src_node_id=? AND branch=? AND kind=?`,
		&sqlitex.ExecOptions{
			Args: []any{id, "main", "calls"},
			ResultFunc: func(stmt *sqlite.Stmt) error {
				_ = stmt.ColumnText(0)
				_ = stmt.ColumnText(1)
				return nil
			},
		})
}

func (b *zbBench) FTSQuery(ctx context.Context, i int) error {
	conn, done, err := b.conn(ctx)
	if err != nil {
		return err
	}
	defer done()
	q := ftsQuery(i)
	if err := sqlitex.Execute(conn,
		`SELECT node_id FROM node_fts_words WHERE node_fts_words MATCH ? LIMIT 50`,
		&sqlitex.ExecOptions{Args: []any{q}, ResultFunc: func(stmt *sqlite.Stmt) error {
			_ = stmt.ColumnText(0)
			return nil
		}}); err != nil {
		return err
	}
	return sqlitex.Execute(conn,
		`SELECT node_id FROM node_fts_trigrams WHERE node_fts_trigrams MATCH ? LIMIT 50`,
		&sqlitex.ExecOptions{Args: []any{q}, ResultFunc: func(stmt *sqlite.Stmt) error {
			_ = stmt.ColumnText(0)
			return nil
		}})
}

func (b *zbBench) RehydrateScan(ctx context.Context) (int, error) {
	conn, done, err := b.conn(ctx)
	if err != nil {
		return 0, err
	}
	defer done()
	n := 0
	err = sqlitex.Execute(conn,
		`SELECT r.node_id, e.embedding
		   FROM node_embedding_refs r
		   JOIN node_embeddings e ON r.content_hash = e.content_hash
		  WHERE r.state = 'ready'`,
		&sqlitex.ExecOptions{ResultFunc: func(stmt *sqlite.Stmt) error {
			_ = stmt.ColumnText(0)
			blobLen := stmt.ColumnLen(1)
			if blobLen == 0 {
				return fmt.Errorf("empty blob")
			}
			buf := make([]byte, blobLen)
			stmt.ColumnBytes(1, buf)
			n++
			return nil
		}})
	return n, err
}

func (b *zbBench) QueuePoll(ctx context.Context, i int) error {
	conn, done, err := b.conn(ctx)
	if err != nil {
		return err
	}
	defer done()
	endFn, err := sqlitex.ImmediateTransaction(conn)
	if err != nil {
		return err
	}
	var commitErr error
	defer func() { endFn(&commitErr) }()

	var seq int64 = -1
	if err := sqlitex.Execute(conn,
		`SELECT seq FROM post_promotion_queue WHERE state='pending' ORDER BY seq LIMIT 1`,
		&sqlitex.ExecOptions{ResultFunc: func(stmt *sqlite.Stmt) error {
			seq = stmt.ColumnInt64(0)
			return nil
		}}); err != nil {
		commitErr = err
		return err
	}
	if seq < 0 {
		if err := sqlitex.Execute(conn, `INSERT INTO post_promotion_queue
			   (promotion_id, repo_id, branch, git_sha, work_kind, payload, state, enqueued_at)
			 VALUES (?,?,?,?,?,?,?,?)`, &sqlitex.ExecOptions{
			Args: []any{fmt.Sprintf("p-iter-%d", i), "bench-repo", "main", "deadbeef",
				"embed", "{}", "pending", int64(1_700_000_000 + i)},
		}); err != nil {
			commitErr = err
			return err
		}
		return nil
	}
	if err := sqlitex.Execute(conn,
		`UPDATE post_promotion_queue SET state='done', completed_at=? WHERE seq=?`,
		&sqlitex.ExecOptions{Args: []any{int64(1_700_000_000 + i), seq}}); err != nil {
		commitErr = err
		return err
	}
	return nil
}

func (b *zbBench) PromotionTx(ctx context.Context, i int) error {
	conn, done, err := b.conn(ctx)
	if err != nil {
		return err
	}
	defer done()
	endFn, err := sqlitex.ImmediateTransaction(conn)
	if err != nil {
		return err
	}
	var commitErr error
	defer func() { endFn(&commitErr) }()

	rng := rand.New(rand.NewSource(int64(i)))
	for k := 0; k < 10; k++ {
		idx := rng.Intn(b.seedCount)
		id := fmt.Sprintf("n%08d", idx)
		ts := int64(1_700_100_000 + i*10 + k)
		if err := sqlitex.Execute(conn,
			`UPDATE nodes SET last_promoted_at=? WHERE node_id=? AND branch=?`,
			&sqlitex.ExecOptions{Args: []any{ts, id, "main"}}); err != nil {
			commitErr = err
			return err
		}
		if err := sqlitex.Execute(conn,
			`UPDATE node_embedding_refs SET state='pending', enqueued_at=? WHERE node_id=?`,
			&sqlitex.ExecOptions{Args: []any{ts, id}}); err != nil {
			commitErr = err
			return err
		}
	}
	if err := sqlitex.Execute(conn,
		`INSERT INTO post_promotion_queue
		   (promotion_id, repo_id, branch, git_sha, work_kind, payload, state, enqueued_at)
		 VALUES (?,?,?,?,?,?,?,?)`,
		&sqlitex.ExecOptions{Args: []any{fmt.Sprintf("prom-%d", i), "bench-repo", "main", "feedface",
			"embed", "{}", "pending", int64(1_700_100_000 + i)}}); err != nil {
		commitErr = err
		return err
	}
	return nil
}

func (b *zbBench) BulkIngest(ctx context.Context, batch int, i int) error {
	conn, done, err := b.conn(ctx)
	if err != nil {
		return err
	}
	defer done()
	endFn, err := sqlitex.ImmediateTransaction(conn)
	if err != nil {
		return err
	}
	var commitErr error
	defer func() { endFn(&commitErr) }()

	for k := 0; k < batch; k++ {
		id := fmt.Sprintf("bi-%d-%d", i, k)
		if err := sqlitex.Execute(conn, `INSERT INTO nodes
			(node_id, branch, repo_id, language, kind, symbol_path, file_path,
			 line_start, line_end, content_hash, last_promoted_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?)`, &sqlitex.ExecOptions{
			Args: []any{id, "main", "bench-repo", "go", "function",
				"bench/" + id, "bench.go", 1, 5, "h" + id, int64(1_700_200_000 + i)},
		}); err != nil {
			commitErr = err
			return err
		}
	}
	return nil
}

func (b *zbBench) Close() error {
	if b.pool == nil {
		return nil
	}
	return b.pool.Close()
}
