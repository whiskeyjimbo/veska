// ReviewTokenStore backs review.DailyTokenStore against the daemon_state
// key-value table. The cumulative per-day review token total lives under the
// key 'review.tokens.<local-date>' as a decimal string — daemon_state is
// runtime/operational state, the correct home for a token counter that must
// survive a daemon restart. Keying on the local date means the window resets
// at local midnight without an explicit reset: a read on a new day finds no
// row and returns zero.
package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"sync"

	"github.com/whiskeyjimbo/veska/internal/application/review"
)

// reviewTokenKeyPrefix is the daemon_state key prefix for the per-day total.
const reviewTokenKeyPrefix = "review.tokens."

// Compile-time assertion that ReviewTokenStore satisfies the port.
var _ review.DailyTokenStore = (*ReviewTokenStore)(nil)

// ReviewTokenStore is the SQLite adapter for review.DailyTokenStore. Reads take
// the read pool; writes take the write pool. The increment is a
// read-modify-write, so a process-local mutex serialises concurrent AddTokens
// calls to keep the running total consistent.
type ReviewTokenStore struct {
	readDB  *sql.DB
	writeDB *sql.DB

	mu sync.Mutex
}

// NewReviewTokenStore constructs a ReviewTokenStore. writeDB carries the
// UPSERT, readDB the lookup.
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

// AddTokens adds n to the running total for localDate and returns the new
// total. The read-modify-write is serialised by a process-local mutex.
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
