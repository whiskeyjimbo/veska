package sqlite

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite/sqldriver"
)

// Pools holds the two *sql.DB handles for a single veska.db file.
//
// ReadDB: unlimited connections, for all read paths.
// Write:  MaxOpenConns=1, the single writer for promotion, MCP writes, and
//
//	the embedder worker. SQLite WAL admits only one writer at the file
//	level, so a second in-process write pool buys nothing but
//	SQLITE_BUSY_SNAPSHOT races on transaction commit (solov2-jtl5.5).
//	Serializing all writes through one Go-level connection lets in-process
//	contention queue on the *sql.DB conn instead of failing mid-tx.
type Pools struct {
	ReadDB *sql.DB
	Write  *sql.DB
}

// OpenPools opens the read and write *sql.DB handles to dbPath with per-role
// PRAGMA setup. Both handles use WAL mode and foreign keys. The write pool
// gets a 30s busy_timeout to absorb the embedder's longer-running writes;
// readers use 5s since they never block writers under WAL.
// Caller must call pools.Close() when done.
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

// openPool opens a single *sql.DB to dbPath with the given MaxOpenConns (0 = unlimited)
// and applies the provided PRAGMA string.
func openPool(dbPath string, maxOpen, busyTimeoutMS int) (*sql.DB, error) {
	// Encode connection-scoped pragmas in the DSN so the driver applies them
	// to EVERY pooled connection. foreign_keys and busy_timeout are per-connection
	// state; the previous one-shot `db.Exec("PRAGMA …")` only set them on a
	// single connection, leaving foreign keys OFF on the rest — so ON DELETE
	// CASCADE silently never fired and `repo remove` orphaned child rows
	// . journal_mode=WAL is persisted in the db file, so encoding
	// it per-connection is harmless.
	db, err := sql.Open(sqldriver.Name, sqldriver.BuildDSN(dbPath, busyTimeoutMS))
	if err != nil {
		return nil, err
	}

	if maxOpen > 0 {
		db.SetMaxOpenConns(maxOpen)
	}

	// Verify foreign_keys actually took on a live connection — a silent OFF
	// here is what this fix exists to prevent.
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
