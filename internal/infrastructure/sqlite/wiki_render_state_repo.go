// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"time"

	"github.com/whiskeyjimbo/veska/internal/application/wiki"
)

const wikiLastRenderKey = "wiki.last_render_at"

var _ wiki.RenderTimeStore = (*WikiRenderStateRepo)(nil)

// WikiRenderStateRepo persists the most recent successful wiki regeneration time
// in the daemon_state table, ensuring it survives daemon restarts.
type WikiRenderStateRepo struct {
	readDB  *sql.DB
	writeDB *sql.DB
}

// NewWikiRenderStateRepo constructs a WikiRenderStateRepo.
func NewWikiRenderStateRepo(readDB, writeDB *sql.DB) *WikiRenderStateRepo {
	return &WikiRenderStateRepo{readDB: readDB, writeDB: writeDB}
}

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

// LastRenderAt returns the most recent persisted render time. Returns false for
// the boolean flag if no render has been recorded yet.
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
