package sqlite

import (
	"database/sql"
	"errors"
	"fmt"

	_ "modernc.org/sqlite"
)

// Pools holds the three *sql.DB handles for a single veska.db file.
// ReadDB: unlimited connections, for all read paths.
// WriteHot: MaxOpenConns=1, for promotion + MCP writes.
// WriteEmbed: MaxOpenConns=1, for embed worker only.
type Pools struct {
	ReadDB     *sql.DB
	WriteHot   *sql.DB
	WriteEmbed *sql.DB
}

// OpenPools opens three *sql.DB handles to dbPath with per-role PRAGMA setup.
// All three handles use WAL mode and foreign keys.
// Caller must call pools.Close() when done.
func OpenPools(dbPath string) (*Pools, error) {
	readDB, err := openPool(dbPath, 0, "PRAGMA journal_mode=WAL; PRAGMA foreign_keys=ON; PRAGMA busy_timeout=5000")
	if err != nil {
		return nil, fmt.Errorf("sqlite.OpenPools ReadDB: %w", err)
	}

	writeHot, err := openPool(dbPath, 1, "PRAGMA journal_mode=WAL; PRAGMA foreign_keys=ON; PRAGMA busy_timeout=5000")
	if err != nil {
		_ = readDB.Close()
		return nil, fmt.Errorf("sqlite.OpenPools WriteHot: %w", err)
	}

	writeEmbed, err := openPool(dbPath, 1, "PRAGMA journal_mode=WAL; PRAGMA foreign_keys=ON; PRAGMA busy_timeout=30000")
	if err != nil {
		_ = readDB.Close()
		_ = writeHot.Close()
		return nil, fmt.Errorf("sqlite.OpenPools WriteEmbed: %w", err)
	}

	return &Pools{
		ReadDB:     readDB,
		WriteHot:   writeHot,
		WriteEmbed: writeEmbed,
	}, nil
}

// openPool opens a single *sql.DB to dbPath with the given MaxOpenConns (0 = unlimited)
// and applies the provided PRAGMA string.
func openPool(dbPath string, maxOpen int, pragmas string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, err
	}

	if maxOpen > 0 {
		db.SetMaxOpenConns(maxOpen)
	}

	if _, err := db.Exec(pragmas); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("apply pragmas: %w", err)
	}

	return db, nil
}

// Close closes all three DB handles, collecting all errors.
func (p *Pools) Close() error {
	var errs []error
	if p.ReadDB != nil {
		if err := p.ReadDB.Close(); err != nil {
			errs = append(errs, fmt.Errorf("ReadDB.Close: %w", err))
		}
	}
	if p.WriteHot != nil {
		if err := p.WriteHot.Close(); err != nil {
			errs = append(errs, fmt.Errorf("WriteHot.Close: %w", err))
		}
	}
	if p.WriteEmbed != nil {
		if err := p.WriteEmbed.Close(); err != nil {
			errs = append(errs, fmt.Errorf("WriteEmbed.Close: %w", err))
		}
	}
	return errors.Join(errs...)
}
