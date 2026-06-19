// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

// Package sqlite provides SQLite implementations of domain repositories.
// ReviewTokenStore backs review.DailyTokenStore against the daemon_state
// key-value table. Storing this in daemon_state ensures the token counter
// survives daemon restarts, while keying on localDate automatically resets
// the daily window at midnight when a read on a new day finds no row.
package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"sync"

	"github.com/whiskeyjimbo/veska/internal/application/review"
)

const reviewTokenKeyPrefix = "review.tokens."

var _ review.DailyTokenStore = (*ReviewTokenStore)(nil)

// ReviewTokenStore is the SQLite adapter for review.DailyTokenStore. The increment
// is a read-modify-write, so a process-local mutex serializes concurrent AddTokens
// calls to keep the running total consistent.
type ReviewTokenStore struct {
	readDB  *sql.DB
	writeDB *sql.DB

	mu sync.Mutex
}

// NewReviewTokenStore constructs a ReviewTokenStore.
func NewReviewTokenStore(readDB, writeDB *sql.DB) *ReviewTokenStore {
	return &ReviewTokenStore{readDB: readDB, writeDB: writeDB}
}

// TokensFor reads the persisted token total for localDate. A date with no
// recorded usage returns 0.
func (s *ReviewTokenStore) TokensFor(ctx context.Context, localDate string) (int, error) {
	var value string
	err := s.readDB.QueryRowContext(ctx,
		`SELECT value FROM daemon_state WHERE key = ?`, reviewTokenKeyPrefix+localDate,
	).Scan(&value)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("review token store: tokens for %q: %w", localDate, err)
	}
	n, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("review token store: parse %q: %w", value, err)
	}
	return n, nil
}

// AddTokens adds n to the running total for localDate and returns the new total.
func (s *ReviewTokenStore) AddTokens(ctx context.Context, localDate string, n int) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	current, err := s.TokensFor(ctx, localDate)
	if err != nil {
		return 0, err
	}
	updated := current + n
	key := reviewTokenKeyPrefix + localDate
	_, err = s.writeDB.ExecContext(ctx, `
		INSERT INTO daemon_state (key, value, set_at)
		VALUES (?, ?, strftime('%s','now'))
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, set_at = excluded.set_at`,
		key, strconv.Itoa(updated),
	)
	if err != nil {
		return 0, fmt.Errorf("review token store: add tokens for %q: %w", localDate, err)
	}
	return updated, nil
}
