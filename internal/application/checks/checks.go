// Package checks contains the synchronous structural-check pipeline that runs
// immediately after a promotion transaction commits. It exposes the Check
// interface, an in-memory Registry, and a Runner that:
//
//  1. invokes every registered Check with the promotion Input,
//  2. persists any returned findings via the FindingStorage port,
//  3. records per-check wall-clock duration on the CheckLatency histogram,
//  4. isolates each Check so an error or panic in one does NOT abort other
//     checks and does NOT propagate back into the promotion path.
//
// Findings are advisory. By the time the Runner is invoked the promotion tx
// has already committed; the Runner therefore never returns an error.
package checks

import (
	"context"
	"sync"
	"time"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
	"github.com/whiskeyjimbo/veska/internal/platform/observability"
)

// Input is the data each Check receives. It identifies the just-promoted slice
// of the graph: the repo, the branch, the git SHA at the head of that branch,
// and the set of file paths that were touched by the promotion.
type Input struct {
	RepoID    string
	Branch    string
	GitSHA    string
	FilePaths []string
	// AddedLines holds the newly-added ("+") lines introduced by the
	// promoted commit, keyed by repo-root-relative file path. The
	// promotion path populates it once; checks that need per-line diff
	// data (e.g. secrets-scan) read it, others ignore it. May be nil.
	AddedLines map[string][]Line
}

// Line is a single newly-added line of a commit's diff: its line number
// in the post-commit revision plus the line text (no leading "+" marker,
// no trailing newline). It mirrors application.Line and git.Line; the
// type is re-declared here so the application package need not import the
// checks sub-package — consistent with how Input mirrors CheckRunInput.
type Line struct {
	Number int
	Text   string
}

// Check is a single structural verification step.
//
// Name returns a stable identifier used as a Prometheus label and in finding
// rule attribution. Names must be unique within a Registry.
//
// Run is invoked once per promotion. It is given the post-commit Input and
// must return zero or more findings. Returning an error is non-fatal: the
// Runner logs the error and continues with the next check.
type Check interface {
	Name() string
	Run(ctx context.Context, in Input) ([]*domain.Finding, error)
}

// AuthoritativeChecker is an optional Check extension declaring that the
// findings returned by Run represent the COMPLETE set of currently-applicable
// findings for a given rule on this repo+branch. The Runner reconciles
// against prior state: any open finding under the declared rule whose
// finding_id is not in the just-returned set is auto-closed with
// reason='revalidated_obsolete'.
//
// VulnScanCheck implements this: it re-resolves the entire dep set on every
// run, so a CVE that no longer applies (because the user bumped the dep)
// must disappear from the findings surface automatically (solov2-jvrc).
// Without this, fixing a vuln leaves the dashboard screaming forever.
//
// AuthoritativeRule returns the rule name to reconcile, or ok=false to
// opt out for this particular Input (e.g. when the check decided to skip
// the scan because the manifest was absent).
type AuthoritativeChecker interface {
	AuthoritativeRule(in Input) (rule string, ok bool)
}

// Registry is a small in-memory map of name → Check.
//
// Registration is expected to happen at daemon start-up; the Registry is not
// optimised for hot-path mutation. It is, however, safe for concurrent reads.
type Registry struct {
	mu     sync.RWMutex
	checks []Check
	names  map[string]struct{}
}

// NewRegistry constructs an empty Registry.
func NewRegistry() *Registry {
	return &Registry{names: make(map[string]struct{})}
}

// Register adds c to the registry. Registering a duplicate name is a no-op so
// callers can re-register defensively at start-up.
func (r *Registry) Register(c Check) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.names[c.Name()]; dup {
		return
	}
	r.names[c.Name()] = struct{}{}
	r.checks = append(r.checks, c)
}

// Names returns the names of the registered checks in registration order. The
// returned slice is a copy, safe to read without holding the registry lock.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, len(r.checks))
	for i, c := range r.checks {
		out[i] = c.Name()
	}
	return out
}

// snapshot returns the current set of checks. The returned slice may be safely
// iterated without holding the registry lock.
func (r *Registry) snapshot() []Check {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Check, len(r.checks))
	copy(out, r.checks)
	return out
}

// Runner dispatches the registered checks against an Input and persists any
// findings via the FindingStorage port.
type Runner struct {
	registry *Registry
	storage  ports.FindingStorage
	metrics  *observability.Metrics
}

// NewRunner constructs a Runner. metrics may be nil — in that case timing is
// silently dropped (useful for embedded callers that do not yet wire metrics).
func NewRunner(reg *Registry, storage ports.FindingStorage, metrics *observability.Metrics) *Runner {
	return &Runner{registry: reg, storage: storage, metrics: metrics}
}

// Run executes every registered Check sequentially. Errors and panics from
// individual checks are caught and isolated: the Runner never returns an error
// because the promotion transaction has already committed by the time it
// fires.
//
// Findings returned by a Check are forwarded to FindingStorage.Save. Storage
// failures are logged via the standard error stream (the Runner does not yet
// take a logger; one will be added when the daemon wires this in) but do not
// abort subsequent checks.
func (r *Runner) Run(ctx context.Context, in Input) {
	if r == nil || r.registry == nil {
		return
	}
	for _, c := range r.registry.snapshot() {
		r.runOne(ctx, c, in)
	}
}

// runOne wraps a single Check invocation in a panic recovery + timer block so
// the rest of the pipeline is unaffected by a misbehaving check.
func (r *Runner) runOne(ctx context.Context, c Check, in Input) {
	start := time.Now()
	defer func() {
		// Swallow any panic — by contract a check failure is non-fatal because
		// the promotion transaction has already committed. A future logger
		// wiring will surface the recovered value; for now the guard alone is
		// what makes the isolation contract hold.
		_ = recover()
		if r.metrics != nil && r.metrics.CheckLatency != nil {
			r.metrics.CheckLatency.
				WithLabelValues(in.RepoID, c.Name()).
				Observe(time.Since(start).Seconds())
		}
	}()

	findings, err := c.Run(ctx, in)
	if err != nil {
		// Advisory: log-and-continue. The promotion has already committed.
		// TODO(m3.01.x): wire a logger so errors are surfaced, not swallowed.
		return
	}
	keep := make([]string, 0, len(findings))
	for _, f := range findings {
		if f == nil {
			continue
		}
		// Storage errors are also non-fatal; the same TODO applies.
		_ = r.storage.Save(ctx, f)
		keep = append(keep, f.FindingID)
	}
	// Authoritative checks (solov2-jvrc): close open findings of the
	// declared rule whose IDs are not in the freshly-returned set, so
	// state that no longer applies (e.g. a vuln resolved by a dep bump)
	// disappears from `veska findings list` without manual cleanup.
	if ac, ok := c.(AuthoritativeChecker); ok && r.storage != nil {
		if rule, on := ac.AuthoritativeRule(in); on && rule != "" {
			_ = r.storage.CloseSupersededByRule(ctx, in.RepoID, in.Branch, rule, keep)
		}
	}
}
