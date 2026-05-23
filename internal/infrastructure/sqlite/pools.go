package sqlite

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"

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
	readDB, err := openPool(dbPath, 0, 5000)
	if err != nil {
		return nil, fmt.Errorf("sqlite.OpenPools ReadDB: %w", err)
	}

	writeHot, err := openPool(dbPath, 1, 5000)
	if err != nil {
		_ = readDB.Close()
		return nil, fmt.Errorf("sqlite.OpenPools WriteHot: %w", err)
	}

	writeEmbed, err := openPool(dbPath, 1, 30000)
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
func openPool(dbPath string, maxOpen, busyTimeoutMS int) (*sql.DB, error) {
	// Encode connection-scoped pragmas in the DSN so modernc applies them to
	// EVERY pooled connection. foreign_keys and busy_timeout are per-connection
	// state; the previous one-shot `db.Exec("PRAGMA …")` only set them on a
	// single connection, leaving foreign keys OFF on the rest — so ON DELETE
	// CASCADE silently never fired and `repo remove` orphaned child rows
	// (solov2-d78r). journal_mode=WAL is persisted in the db file, so encoding
	// it per-connection is harmless.
	db, err := sql.Open("sqlite", poolDSN(dbPath, busyTimeoutMS))
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

// poolDSN builds a modernc.org/sqlite DSN that enables WAL, foreign keys, and a
// busy_timeout on every connection the pool opens. dbPath may be a plain
// filesystem path or an existing file: DSN with query params (e.g. an
// in-memory shared-cache handle); the pragmas are appended either way.
func poolDSN(dbPath string, busyTimeoutMS int) string {
	base := dbPath
	if !strings.HasPrefix(base, "file:") {
		base = "file:" + base
	}
	sep := "?"
	if strings.Contains(base, "?") {
		sep = "&"
	}
	q := url.Values{}
	q.Add("_pragma", "journal_mode=WAL")
	q.Add("_pragma", "foreign_keys=on")
	q.Add("_pragma", fmt.Sprintf("busy_timeout=%d", busyTimeoutMS))
	return base + sep + q.Encode()
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
