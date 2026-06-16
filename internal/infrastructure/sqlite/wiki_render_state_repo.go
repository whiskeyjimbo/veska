// WikiRenderStateRepo backs wiki.RenderTimeStore against the daemon_state
// key-value table. The most recent successful wiki regeneration time lives
// under the key 'wiki.last_render_at' as a Unix-millisecond string
// daemon_state is runtime/operational state, the correct home (vs
// database_meta which holds schema metadata).

package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"time"

	"github.com/whiskeyjimbo/veska/internal/application/wiki"
)

// wikiLastRenderKey is the daemon_state key the last-render time is stored
// under.
const wikiLastRenderKey = "wiki.last_render_at"

// Compile-time assertion that WikiRenderStateRepo satisfies the port.
var _ wiki.RenderTimeStore = (*WikiRenderStateRepo)(nil)

// WikiRenderStateRepo is the SQLite adapter for wiki.RenderTimeStore. Reads
// take the read pool; writes take the write pool. Both *sql.DB handles are
// safe for concurrent use, so the repo itself is too.
type WikiRenderStateRepo struct {
	readDB  *sql.DB
	writeDB *sql.DB
}

// NewWikiRenderStateRepo constructs a WikiRenderStateRepo. writeDB carries
// the UPSERT, readDB the lookup.
func NewWikiRenderStateRepo(readDB, writeDB *sql.DB) *WikiRenderStateRepo {
	return &WikiRenderStateRepo{readDB: readDB, writeDB: writeDB}
}

// SetLastRenderAt upserts the last-render time keyed on wiki.last_render_at.
func (r *WikiRenderStateRepo) SetLastRenderAt(ctx context.Context, t time.Time) error {
	value := strconv.FormatInt(t.UnixMilli(), 10)
	_, err := r.writeDB.ExecContext(ctx, `
		INSERT INTO daemon_state (key, value, set_at)
		VALUES (?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, set_at = excluded.set_at`,
		wikiLastRenderKey, value, t.UnixMilli(),
	)
	if err != nil {
		return fmt.Errorf("wiki render state: set last render at: %w", err)
	}
	return nil
}

// LastRenderAt reads the most recent persisted render time. The bool is
// false when no render has been recorded yet.
func (r *WikiRenderStateRepo) LastRenderAt(ctx context.Context) (time.Time, bool, error) {
	var value string
	err := r.readDB.QueryRowContext(ctx,
		`SELECT value FROM daemon_state WHERE key = ?`, wikiLastRenderKey,
	).Scan(&value)
	if err == sql.ErrNoRows {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("wiki render state: last render at: %w", err)
	}
	ms, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("wiki render state: parse %q: %w", value, err)
	}
	return time.UnixMilli(ms), true, nil
}
