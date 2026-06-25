// SPDX-License-Identifier: AGPL-3.0-only

package checks

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/whiskeyjimbo/veska/internal/application/cycles"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

// PackageDepGraphReader returns the module-internal package dependency graph for
// (repoID, branch): importer package dir -> imported package dirs. It is the
// narrow capability ImportCycleCheck needs; sqlite.PackageDepsRepo satisfies it.
type PackageDepGraphReader interface {
	PackageDependencies(ctx context.Context, repoID, branch string) (map[string][]string, error)
}

// ImportCycleCheck flags strongly-connected components of >= 2 packages in the
// internal import graph - i.e. package import cycles. It is zero-config (a cycle
// is a cycle on any repo) and runs repo-wide every promotion: a cycle is a
// global property of the graph, so it cannot be scoped to the changed files. As
// an AuthoritativeChecker it re-resolves the complete cycle set each run, so a
// cycle that is broken by a later commit auto-closes.
//
// In Go, package import cycles are a compile error, so on a building Go repo
// this should find nothing - its value is catching a cycle the moment it is
// introduced (before the build is attempted) and serving languages where the
// compiler permits import cycles.
type ImportCycleCheck struct {
	graph PackageDepGraphReader
	// repoKind, when set, returns a repo's kind; ephemeral cache-tier clones
	// short-circuit to zero findings, mirroring the dead-code / untested checks
	// (reporting cycles in an external library's code is noise).
	repoKind func(ctx context.Context, repoID string) (string, error)
}

// ImportCycleOption configures an ImportCycleCheck.
type ImportCycleOption func(*ImportCycleCheck)

// WithImportCycleRepoKindLookup wires a callback returning a repo's kind
// ("tracked" / "ephemeral"); Run skips reporting on ephemeral repos.
func WithImportCycleRepoKindLookup(fn func(ctx context.Context, repoID string) (string, error)) ImportCycleOption {
	return func(c *ImportCycleCheck) { c.repoKind = fn }
}

// NewImportCycleCheck constructs an ImportCycleCheck over the package-dependency
// reader. The reader is required; a nil reader makes Run return an error.
func NewImportCycleCheck(graph PackageDepGraphReader, opts ...ImportCycleOption) *ImportCycleCheck {
	c := &ImportCycleCheck{graph: graph}
	for _, o := range opts {
		o(c)
	}
	return c
}

// Name returns the finding-rule / Prometheus attribution name.
func (c *ImportCycleCheck) Name() string { return "import-cycle" }

// AuthoritativeRule declares that Run returns the complete set of import cycles,
// so the Runner auto-closes any prior import-cycle finding no longer present.
func (c *ImportCycleCheck) AuthoritativeRule(in Input) (string, bool) {
	return "import-cycle", true
}

// Run loads the internal package graph, finds every SCC of >= 2 packages, and
// emits one finding per cycle. FilePaths is ignored: a cycle is global, and the
// authoritative reconciliation needs the full set regardless of what changed.
func (c *ImportCycleCheck) Run(ctx context.Context, in Input) ([]*domain.Finding, error) {
	if c == nil || c.graph == nil {
		return nil, fmt.Errorf("import-cycle: nil package graph reader")
	}
	if c.repoKind != nil {
		// Lookup errors fail open (report on a tracked repo rather than silently
		// suppress during a registry hiccup), matching the sibling checks.
		if kind, err := c.repoKind(ctx, in.RepoID); err == nil && kind == "ephemeral" {
			return nil, nil
		}
	}

	adj, err := c.graph.PackageDependencies(ctx, in.RepoID, in.Branch)
	if err != nil {
		return nil, fmt.Errorf("import-cycle: load package graph: %w", err)
	}

	edges := make([]cycles.Edge, 0, len(adj))
	for src, dsts := range adj {
		for _, dst := range dsts {
			edges = append(edges, cycles.Edge{Src: src, Dst: dst})
		}
	}

	var out []*domain.Finding
	for _, scc := range cycles.StronglyConnected(edges) {
		if len(scc) < 2 {
			continue // not a cycle
		}
		sort.Strings(scc)
		f, err := newImportCycleFinding(in, scc)
		if err != nil {
			continue // a malformed anchor should not abort the whole check
		}
		out = append(out, f)
	}
	return out, nil
}

// newImportCycleFinding builds the finding for one cycle. The cycle's identity is
// the sorted package set (folded into the finding key) so the finding_id is
// stable across runs regardless of which member is chosen as the anchor; the
// anchor is the lexicographically smallest package dir so the finding has a
// concrete location.
func newImportCycleFinding(in Input, members []string) (*domain.Finding, error) {
	msg := fmt.Sprintf("import cycle among %d packages: %s (mutually dependent)",
		len(members), strings.Join(members, " -> "))
	return domain.NewFinding(domain.FindingSpec{
		RepoID:   in.RepoID,
		Branch:   in.Branch,
		Severity: domain.SeverityMedium,
		Layer:    domain.LayerStructural,
		Rule:     "import-cycle",
		Message:  msg,
	},
		domain.WithFileAnchor(members[0]),
		domain.WithFindingKey(strings.Join(members, "|")),
	)
}
