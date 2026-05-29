package checks

import (
	"context"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/whiskeyjimbo/veska/internal/application/pathfilter"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// DeadCodeCheck is a structural check that flags nodes with no inbound edges
// on the promotion branch. "No inbound edges" is a necessary-but-not-sufficient
// signal of deadness — the symbol could still be called by an external caller
// the graph cannot see (entry points, exported APIs, framework callbacks, test
// helpers). To minimise false positives the check skips a small allowlist of
// well-known external-entry shapes:
//
//   - Functions/methods named main or init.
//   - Names beginning with Test, Example, or Benchmark (Go test runner).
//   - Names whose first rune is uppercase (Go-exported / TS-public, conservatively).
//
// These filters live in the application layer, NOT in the adapter SQL, so the
// rules remain easy to evolve and trivial to unit-test without a database.
type DeadCodeCheck struct {
	q ports.DeadCodeQuerier
	// repoKind, when non-nil, returns the kind ("tracked" / "ephemeral")
	// of a given repoID. Ephemeral cache-tier clones (registered by
	// `veska search --repo <url>`) short-circuit to zero findings — the
	// user is exploring an external codebase, not curating its findings,
	// and a 75-file pflag clone otherwise emits ~220 low-severity
	// dead-code findings on the upstream public API (solov2-izh6.13).
	// When unset, behaviour is unchanged.
	repoKind func(ctx context.Context, repoID string) (string, error)
}

// DeadCodeOption configures a DeadCodeCheck. None are required today; the
// type is here so future cross-cutting concerns can land without a breaking
// constructor change.
type DeadCodeOption func(*DeadCodeCheck)

// WithDeadCodeRepoKindLookup wires a callback that returns a repo's Kind
// ("tracked" / "ephemeral"). Used by Run to skip dead-code reporting on
// ephemeral repos — siblings the autolink short-circuit added in
// solov2-izh6.8.
func WithDeadCodeRepoKindLookup(fn func(ctx context.Context, repoID string) (string, error)) DeadCodeOption {
	return func(c *DeadCodeCheck) { c.repoKind = fn }
}

// NewDeadCodeCheck constructs a DeadCodeCheck bound to q. The querier is
// required; passing nil will cause Run to return an error on first invocation.
func NewDeadCodeCheck(q ports.DeadCodeQuerier, opts ...DeadCodeOption) *DeadCodeCheck {
	c := &DeadCodeCheck{q: q}
	for _, o := range opts {
		o(c)
	}
	return c
}

// Name returns the Prometheus / finding-rule attribution name.
func (c *DeadCodeCheck) Name() string { return "dead-code" }

// Run loads the set of dead nodes from the querier for the input's file paths,
// applies the external-entry allowlist filters, and constructs one Finding per
// surviving node. Findings are anchored on node_id which makes finding_id
// branch-stable and idempotent under the underlying ON CONFLICT clause.
//
// An empty Input.FilePaths is a no-op: the querier is still consulted (it must
// return empty) but no findings are produced.
func (c *DeadCodeCheck) Run(ctx context.Context, in Input) ([]*domain.Finding, error) {
	if c == nil || c.q == nil {
		return nil, fmt.Errorf("dead-code: nil querier")
	}
	if len(in.FilePaths) == 0 {
		return nil, nil
	}
	// solov2-izh6.13: ephemeral repos (cache-tier clones from
	// `veska search --repo <url>`) skip dead-code entirely. Reporting
	// "unused" symbols on an external library's public API trains the
	// junior to ignore the findings surface from day one. Lookup errors
	// fail open (over-report rather than silently suppress on a tracked
	// repo when the registry briefly hiccups).
	if c.repoKind != nil {
		if kind, err := c.repoKind(ctx, in.RepoID); err == nil && kind == "ephemeral" {
			return nil, nil
		}
	}

	dead, err := c.q.DeadNodesInFiles(ctx, in.RepoID, in.Branch, in.FilePaths)
	if err != nil {
		return nil, fmt.Errorf("dead-code: query: %w", err)
	}

	// solov2-f1zp: pull the bare names of every interface method declared
	// in this repo so concrete implementations (boolValue.Set satisfies
	// pflag.Value's Set) are not reported as dead. Interface dispatch
	// emits no CALLS edges the static graph can see; without this filter
	// almost every method on a Value-shaped type in any library repo
	// (the junior journey hit 220 on pflag) shows up as "no inbound
	// edges". An error here fails open — we'd rather over-report than
	// suppress real findings on a tracked repo when the registry briefly
	// hiccups.
	var ifaceMethods map[string]struct{}
	if names, ierr := c.q.InterfaceMethodNames(ctx, in.RepoID, in.Branch); ierr == nil && len(names) > 0 {
		ifaceMethods = make(map[string]struct{}, len(names))
		for _, n := range names {
			ifaceMethods[n] = struct{}{}
		}
	}

	out := make([]*domain.Finding, 0, len(dead))
	for _, n := range dead {
		if !isDeadCodeCandidate(n) || isExternalEntry(n) || isTestFile(n.FilePath) || pathfilter.IsVendored(n.FilePath) {
			continue
		}
		if n.Kind == "method" && isInterfaceMethodImpl(n.Name, ifaceMethods) {
			continue
		}
		msg := fmt.Sprintf("symbol %q in %s has no inbound edges on branch %s",
			n.Name, n.FilePath, in.Branch)
		// Capture the dead node's content_hash on the finding so the
		// revalidation sweep (m3.05.2) can detect drift. An empty hash from
		// the adapter (older rows pre-content-hash) is left off the option
		// list so the stored column stays NULL.
		opts := []domain.FindingOption{domain.WithNodeAnchor(n.NodeID)}
		if n.ContentHash != "" {
			opts = append(opts, domain.WithAnchorContentHash(n.ContentHash))
		}
		f, err := domain.NewFinding(
			in.RepoID, in.Branch,
			domain.SeverityLow,
			domain.LayerStructural,
			"dead-code",
			msg,
			opts...,
		)
		if err != nil {
			// A malformed node ref should not abort the whole check; skip it.
			continue
		}
		out = append(out, f)
	}
	return out, nil
}

// deadCodeKinds is the set of node kinds for which "no inbound edges" is a
// meaningful deadness signal — callable or referenceable symbols. Container
// and sub-symbol kinds (package, file, module, chunk, field, …) carry no
// inbound CALLS/REFERENCES edges by construction, so flagging them produced
// pure noise (solov2-xpb). Anything not in this set is never reported.
var deadCodeKinds = map[string]bool{
	"function":  true,
	"method":    true,
	"type":      true,
	"struct":    true,
	"interface": true,
	"class":     true,
}

// isDeadCodeCandidate reports whether n's kind is one the dead-code check
// should reason about. Unknown/empty kinds are excluded conservatively.
func isDeadCodeCandidate(n ports.NodeRef) bool {
	return deadCodeKinds[n.Kind]
}

// isTestFile reports whether path is a unit-test source by file-name
// convention across the languages veska indexes. Test-only helpers
// (fixtures, mocks, table builders) are commonly referenced only by
// their tests and as function values — neither of which produces a
// CALLS edge today (solov2-ix3k). Skipping symbols defined in test
// files cuts a noisy class of false positives without weakening the
// signal for production code.
func isTestFile(path string) bool {
	if path == "" {
		return false
	}
	base := path
	if i := strings.LastIndexAny(path, "/\\"); i >= 0 {
		base = path[i+1:]
	}
	switch {
	case strings.HasSuffix(base, "_test.go"): // Go
		return true
	case strings.HasPrefix(base, "test_") && strings.HasSuffix(base, ".py"): // pytest
		return true
	case strings.HasSuffix(base, "_test.py"): // pytest alt
		return true
	case strings.HasSuffix(base, ".test.ts"), strings.HasSuffix(base, ".test.tsx"),
		strings.HasSuffix(base, ".test.js"), strings.HasSuffix(base, ".test.jsx"),
		strings.HasSuffix(base, ".spec.ts"), strings.HasSuffix(base, ".spec.tsx"),
		strings.HasSuffix(base, ".spec.js"), strings.HasSuffix(base, ".spec.jsx"):
		return true
	}
	return false
}

// isInterfaceMethodImpl reports whether name is a method whose bare
// suffix matches one of the interface method names declared in the
// same repo. Used by Run to skip dead-code reports on methods that
// likely satisfy an interface contract — interface dispatch produces
// no CALLS edge the static graph can see (solov2-f1zp). Names without
// a '.' (orphan methods) and an empty ifaceMethods map are no-ops.
func isInterfaceMethodImpl(name string, ifaceMethods map[string]struct{}) bool {
	if len(ifaceMethods) == 0 || name == "" {
		return false
	}
	dot := strings.LastIndex(name, ".")
	if dot < 0 || dot == len(name)-1 {
		return false
	}
	_, ok := ifaceMethods[name[dot+1:]]
	return ok
}

// isExternalEntry reports whether n looks like a node that could be invoked by
// something outside the graph the resolver can see. These are conservatively
// excluded from the dead-code report.
func isExternalEntry(n ports.NodeRef) bool {
	name := n.Name
	if name == "" {
		// No name to reason about — be conservative and treat as external.
		return true
	}

	// Entry-point hooks on function/method-kind nodes.
	if n.Kind == "function" || n.Kind == "method" {
		if name == "main" || name == "init" {
			return true
		}
	}
	// Names like 'init' on any kind are framework hooks in several languages;
	// keep the rule generic across kinds for safety.
	if name == "main" || name == "init" {
		return true
	}

	// Go test-runner prefixes.
	if strings.HasPrefix(name, "Test") ||
		strings.HasPrefix(name, "Example") ||
		strings.HasPrefix(name, "Benchmark") {
		return true
	}

	// Exported / public symbols: first rune is uppercase letter.
	r, _ := utf8.DecodeRuneInString(name)
	if r != utf8.RuneError && unicode.IsUpper(r) {
		return true
	}
	return false
}
