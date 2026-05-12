// Package loader provides a SQLite + sqlite-vec loader for synthetic vectors.
// It creates a vec0 virtual table (vec_nodes) and inserts batches of float32 vectors,
// recording load wall-clock time, on-disk size, and peak RSS.
package loader

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	vec "github.com/asg017/sqlite-vec-go-bindings/cgo"
	_ "github.com/mattn/go-sqlite3"
)

func init() {
	vec.Auto()
}

// LoadMetrics holds timing and resource measurements for a single load pass.
type LoadMetrics struct {
	Population   int64 `json:"population"`
	LoadWallMs   int64 `json:"load_wall_ms"`
	DiskBytes    int64 `json:"disk_bytes"`
	PeakRSSBytes int64 `json:"peak_rss_bytes"`
}

// Loader wraps a SQLite connection with sqlite-vec loaded.
type Loader struct {
	db   *sql.DB
	path string
}

// Open opens (or creates) a SQLite database at path with the default page size.
func Open(path string) (*Loader, error) {
	return OpenWithPageSize(path, 0)
}

// OpenWithPageSize opens (or creates) a SQLite database at path.
// pageSize must be a power of two in [512, 65536], or 0 for the SQLite default (4096).
// The pragma is only effective on a new database; it is silently ignored on existing ones.
func OpenWithPageSize(path string, pageSize int) (*Loader, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("loader: mkdir: %w", err)
	}

	db, err := sql.Open("sqlite3", path+"?_journal_mode=WAL")
	if err != nil {
		return nil, fmt.Errorf("loader: open db: %w", err)
	}
	db.SetMaxOpenConns(1)

	if pageSize > 0 {
		if _, err := db.Exec(fmt.Sprintf(`PRAGMA page_size = %d`, pageSize)); err != nil {
			db.Close()
			return nil, fmt.Errorf("loader: set page_size %d: %w", pageSize, err)
		}
	}

	if _, err := db.Exec(`CREATE VIRTUAL TABLE IF NOT EXISTS vec_nodes USING vec0(embedding FLOAT[768])`); err != nil {
		db.Close()
		return nil, fmt.Errorf("loader: create vec_nodes: %w", err)
	}

	return &Loader{db: db, path: path}, nil
}

// InsertBatch inserts a batch of 768-dim vectors into the vec_nodes table.
// It uses a single transaction for efficiency.
func (l *Loader) InsertBatch(vecs [][]float32) error {
	tx, err := l.db.Begin()
	if err != nil {
		return fmt.Errorf("loader: begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	stmt, err := tx.Prepare(`INSERT INTO vec_nodes(embedding) VALUES (?)`)
	if err != nil {
		return fmt.Errorf("loader: prepare insert: %w", err)
	}
	defer stmt.Close()

	for i, v := range vecs {
		blob, err := vec.SerializeFloat32(v)
		if err != nil {
			return fmt.Errorf("loader: serialize vec[%d]: %w", i, err)
		}
		if _, err := stmt.Exec(blob); err != nil {
			return fmt.Errorf("loader: insert vec[%d]: %w", i, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("loader: commit: %w", err)
	}
	return nil
}

// RowCount returns the number of rows in the vec_nodes virtual table.
func (l *Loader) RowCount() (int64, error) {
	var count int64
	if err := l.db.QueryRow(`SELECT count(*) FROM vec_nodes`).Scan(&count); err != nil {
		return 0, fmt.Errorf("loader: row count: %w", err)
	}
	return count, nil
}

// TableExists reports whether a table (or virtual table) with the given name exists.
func (l *Loader) TableExists(name string) (bool, error) {
	var count int
	err := l.db.QueryRow(
		`SELECT count(*) FROM sqlite_master WHERE type IN ('table','shadow') AND name = ?`,
		name,
	).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("loader: table exists query: %w", err)
	}
	if count > 0 {
		return true, nil
	}
	// Also check sqlite_schema for virtual tables (vec0 registers differently).
	err = l.db.QueryRow(
		`SELECT count(*) FROM sqlite_master WHERE name = ?`,
		name,
	).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("loader: virtual table exists query: %w", err)
	}
	return count > 0, nil
}

// Checkpoint runs a full WAL checkpoint so all data is flushed to the main DB file.
func (l *Loader) Checkpoint() error {
	if _, err := l.db.Exec(`PRAGMA wal_checkpoint(FULL)`); err != nil {
		return fmt.Errorf("loader: checkpoint: %w", err)
	}
	return nil
}

// DiskBytes returns the size in bytes of the main SQLite database file.
// Call Checkpoint first to flush WAL data into the main file.
func (l *Loader) DiskBytes() (int64, error) {
	info, err := os.Stat(l.path)
	if err != nil {
		return 0, fmt.Errorf("loader: stat db file: %w", err)
	}
	return info.Size(), nil
}

// Close closes the underlying database connection.
func (l *Loader) Close() error {
	return l.db.Close()
}


// WriteMetricsJSON writes a JSON array of LoadMetrics to the given file path,
// creating parent directories as needed.
func WriteMetricsJSON(path string, metrics []LoadMetrics) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("loader: mkdir for metrics: %w", err)
	}
	data, err := json.MarshalIndent(metrics, "", "  ")
	if err != nil {
		return fmt.Errorf("loader: marshal metrics: %w", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("loader: write metrics file: %w", err)
	}
	return nil
}
