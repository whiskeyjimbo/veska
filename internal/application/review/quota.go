package review

import (
	"context"
	"sync"
	"time"
)

// BudgetRule is the rule string carried by every BudgetExceeded Finding. It is
// distinct from FailureRule (the review-pipeline-failure contract): a budget
// refusal is a cost-cap event, not a pipeline fault.
const BudgetRule = "budget-exceeded"

// quotaExceeded is the degraded_reason / structured-error tag carried when a
// review job is refused for a token-budget overage.
const quotaExceeded = "review_quota_exceeded"

// DailyTokenStore persists the cumulative per-day review token total. The
// total is keyed by a local-date string so a read on a new day returns zero
// without an explicit reset. Implementations are backed by daemon_state.
type DailyTokenStore interface {
	// TokensFor returns the persisted token total for the given local date
	// (format "2006-01-02"). A date with no recorded usage returns 0.
	TokensFor(ctx context.Context, localDate string) (int, error)

	// AddTokens adds n to the running total for localDate and returns the new
	// total. Implementations must apply the increment atomically.
	AddTokens(ctx context.Context, localDate string, n int) (int, error)
}

// Quota enforces the review token caps. The per-commit total is held in memory
// (a promotion's review jobs run close together); the per-day total is
// persisted via a DailyTokenStore so it survives a daemon restart. A cap of
// zero means "unlimited" — the corresponding check never trips.
//
// Quota is safe for concurrent use.
type Quota struct {
	maxPerCommit int
	maxPerDay    int
	now          func() time.Time
	store        DailyTokenStore

	mu        sync.Mutex
	perCommit map[string]int
}

// NewQuota constructs a Quota. maxPerCommit and maxPerDay come from
// config.ReviewConfig; a value <= 0 disables that cap. store persists the
// daily total. now defaults to time.Now when nil — tests inject a fixed clock
// to exercise the local-midnight window reset.
func NewQuota(maxPerCommit, maxPerDay int, store DailyTokenStore, now func() time.Time) *Quota {
	if now == nil {
		now = time.Now
	}
	return &Quota{
		maxPerCommit: maxPerCommit,
		maxPerDay:    maxPerDay,
		now:          now,
		store:        store,
		perCommit:    make(map[string]int),
	}
}

// localDate is the "2006-01-02" key for the current local day. The window
// resets at local midnight purely because the date string changes.
func (q *Quota) localDate() string {
	return q.now().Format("2006-01-02")
}

// DailyPaused reports whether the cumulative daily token total has reached the
// configured per-day cap. When true the review handler must not dispatch new
// jobs. A maxPerDay <= 0 disables the cap and this always returns false.
func (q *Quota) DailyPaused(ctx context.Context) (bool, int, error) {
	if q.maxPerDay <= 0 {
		return false, 0, nil
	}
	total, err := q.store.TokensFor(ctx, q.localDate())
	if err != nil {
		return false, 0, err
	}
	return total >= q.maxPerDay, total, nil
}

// CommitExceeded reports whether the running per-commit total for gitSHA has
// reached the configured per-commit cap. When true the remaining review jobs
// for that commit must be refused. A maxPerCommit <= 0 disables the cap.
func (q *Quota) CommitExceeded(gitSHA string) bool {
	if q.maxPerCommit <= 0 {
		return false
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.perCommit[gitSHA] >= q.maxPerCommit
}

// Record adds tokens consumed by a completed review job to both the per-commit
// (in-memory) and per-day (persisted) totals. It is called post-hoc, after the
// LLM call returns its actual usage.
func (q *Quota) Record(ctx context.Context, gitSHA string, tokens int) error {
	if tokens < 0 {
		tokens = 0
	}
	q.mu.Lock()
	q.perCommit[gitSHA] += tokens
	q.mu.Unlock()

	if _, err := q.store.AddTokens(ctx, q.localDate(), tokens); err != nil {
		return err
	}
	return nil
}

// TokensToday returns the persisted cumulative token total for the current
// local day. It is the value the doctor pipelines probe reports.
func (q *Quota) TokensToday(ctx context.Context) (int, error) {
	return q.store.TokensFor(ctx, q.localDate())
}
