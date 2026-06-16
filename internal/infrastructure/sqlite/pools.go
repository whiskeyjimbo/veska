package sqlite

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite/sqldriver"
)

// Pools manages separate read and write database handles for a SQLite database.
// SQLite WAL allows only one writer at a time, so the write pool is restricted
// to one connection to serialize writes in Go rather than letting concurrent
// transactions fail with SQLITE_BUSY_SNAPSHOT errors.
type Pools struct {
	ReadDB *sql.DB
	Write  *sql.DB
}

// OpenPools opens read and write database handles with per-role timeouts. The
// write pool uses a 30-second busy timeout to absorb longer embedder writes,
// while the read pool uses a 5-second timeout because readers do not block
// writers under WAL.
func OpenPools(dbPath string) (*Pools, error) {
	readDB, err := openPool(dbPath, 0, 5000)
	if err != nil {
		return nil, fmt.Errorf("sqlite.OpenPools ReadDB: %w", err)
	}

	write, err := openPool(dbPath, 1, 30000)
	if err != nil {
		_ = readDB.Close()
		return nil, fmt.Errorf("sqlite.OpenPools Write: %w", err)
	}

	return &Pools{
		ReadDB: readDB,
		Write:  write,
	}, nil
}

// openPool opens a single *sql.DB to dbPath with the given MaxOpenConns (0 = unlimited).
func openPool(dbPath string, maxOpen, busyTimeoutMS int) (*sql.DB, error) {
	// Connection-scoped pragmas must be encoded in the DSN rather than executed
	// via db.Exec so they apply to all connections in the pool. This ensures
	// foreign key enforcement (such as ON DELETE CASCADE) remains active across
	// all connections.
	db, err := sql.Open(sqldriver.Name, sqldriver.BuildDSN(dbPath, busyTimeoutMS))
	if err != nil {
		return nil, err
	}

	if maxOpen > 0 {
		db.SetMaxOpenConns(maxOpen)
	}

	// Verify that foreign key enforcement is active on a live connection to
	// prevent silent cascading failures.
	var fk int
	if err := db.QueryRow(`PRAGMA foreign_keys`).Scan(&fk); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("check foreign_keys: %w", err)
	}
	if fk != 1 {
		_ = db.Close()
		return nil, fmt.Errorf("foreign_keys not enabled on pool connection (got %d)", fk)
	}

	return db, nil
}

// Close closes both DB handles, collecting all errors.
func (p *Pools) Close() error {
	var errs []error
	if p.ReadDB != nil {
		if err := p.ReadDB.Close(); err != nil {
			errs = append(errs, fmt.Errorf("ReadDB.Close: %w", err))
		}
	}
	if p.Write != nil {
		if err := p.Write.Close(); err != nil {
			errs = append(errs, fmt.Errorf("Write.Close: %w", err))
		}
	}
	return errors.Join(errs...)
}
