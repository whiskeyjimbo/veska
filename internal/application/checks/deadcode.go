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
}

// NewDeadCodeCheck constructs a DeadCodeCheck bound to q. The querier is
// required; passing nil will cause Run to return an error on first invocation.
func NewDeadCodeCheck(q ports.DeadCodeQuerier) *DeadCodeCheck {
	return &DeadCodeCheck{q: q}
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

	dead, err := c.q.DeadNodesInFiles(ctx, in.RepoID, in.Branch, in.FilePaths)
	if err != nil {
		return nil, fmt.Errorf("dead-code: query: %w", err)
	}

	out := make([]*domain.Finding, 0, len(dead))
	for _, n := range dead {
		if !isDeadCodeCandidate(n) || isExternalEntry(n) || isTestFile(n.FilePath) || pathfilter.IsVendored(n.FilePath) {
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
