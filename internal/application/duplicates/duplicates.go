// Package duplicates finds exact-clone groups: sets of >=2 nodes whose
// content_hash is byte-identical, i.e. literal copy-paste.
//
// This is the deterministic, embedding-free half of duplicate detection
// (solov2-wfrj). content_hash is sha256 of a node's verbatim declaration
// bytes (see domain.Node / treesitter), so two functions collide here only
// when their source text is identical character-for-character — exactly the
// "exact clone" the autolink SIMILAR_TO path treats as merely "related".
//
// Near-duplicate clustering (a higher-threshold re-slice of the SIMILAR_TO
// edges autolink already persists) is deliberately NOT here: those edges carry
// no per-edge similarity score today, so honest near-dup detection needs a
// score-on-edge migration first. That is tracked as a separate follow-up
// (solov2-c1s4); this package ships the exact half only.
//
// For the single-function question "is THIS function duplicated?", use
// eng_search_similar / `veska similar <symbol>` — that is the vector-neighbour
// pivot and needs no group-wide scan.
package duplicates

import (
	"context"
	"errors"
	"fmt"
	"sort"
)

// ExcludedKinds are container / sub-symbol node kinds for which a content_hash
// collision carries no refactor signal: package/file/module nodes hash whole
// regions, chunk nodes are synthetic windows, and field/import nodes are tiny
// fragments that collide constantly across files. The set mirrors autolink's
// nonSymbolKinds (internal/application/autolink) so both duplicate-detection
// surfaces filter identically. A blocklist (rather than a symbol allowlist)
// keeps unknown or future symbol kinds eligible by default.
var ExcludedKinds = []string{"package", "chunk", "file", "module", "field", "import"}

// ClonedNode is one node that shares its content_hash with at least one other
// node in the same (repo, branch). The CloneStore returns these flat; the
// Finder folds them into CloneGroups.
type ClonedNode struct {
	ContentHash string
	NodeID      string
	SymbolPath  string
	FilePath    string
	Kind        string
	LineStart   int
	LineEnd     int
}

// CloneMember is one occurrence of a clone within a CloneGroup.
type CloneMember struct {
	NodeID     string
	SymbolPath string
	FilePath   string
	Kind       string
	LineStart  int
	LineEnd    int
}

// CloneGroup is a set of >=2 nodes sharing one content_hash — N literal copies
// of the same code. Size is len(Members), surfaced explicitly so callers can
// rank "most-copied" groups without re-counting.
type CloneGroup struct {
	ContentHash string
	Size        int
	Members     []CloneMember
}

// CloneStore is the narrow port the Finder needs: it returns every node whose
// content_hash is shared by >=2 nodes in (repoID, branch), excluding
// excludeKinds. Grouping and ordering are the Finder's responsibility, so the
// store may return rows in any order.
type CloneStore interface {
	ClonedNodes(ctx context.Context, repoID, branch string, excludeKinds []string) ([]ClonedNode, error)
}

// ErrMissingDependency is returned by NewFinder when a required collaborator is
// nil. errors.Is-matchable so callers distinguish a wiring fault from a runtime
// failure, mirroring the autolink / promoter constructors.
var ErrMissingDependency = errors.New("duplicates: missing required dependency")

// Finder computes exact-clone groups. The zero value is not usable; construct
// with NewFinder.
type Finder struct {
	store CloneStore
}

// NewFinder constructs a Finder. store is required: a nil dependency yields an
// error wrapping ErrMissingDependency and a nil *Finder.
func NewFinder(store CloneStore) (*Finder, error) {
	if store == nil {
		return nil, fmt.Errorf("duplicates.NewFinder: store is nil: %w", ErrMissingDependency)
	}
	return &Finder{store: store}, nil
}

// ExactClones returns the content_hash clone groups in (repoID, branch),
// excluding container/sub-symbol kinds. Every returned group has Size >= 2.
//
// Ordering is deterministic: groups by descending Size then ascending
// ContentHash (most-copied first, stable tie-break); members within a group by
// (FilePath, LineStart) so the same physical layout always renders the same.
func (f *Finder) ExactClones(ctx context.Context, repoID, branch string) ([]CloneGroup, error) {
	rows, err := f.store.ClonedNodes(ctx, repoID, branch, ExcludedKinds)
	if err != nil {
		return nil, fmt.Errorf("duplicates.ExactClones: %w", err)
	}
	return groupByHash(rows), nil
}

// groupByHash folds flat ClonedNode rows into deterministically-ordered
// CloneGroups, dropping any hash that ended up with a single member (defensive:
// the store already enforces COUNT>=2, but grouping here keeps the invariant
// local and lets the store stay a dumb projection).
func groupByHash(rows []ClonedNode) []CloneGroup {
	byHash := make(map[string][]CloneMember)
	order := make([]string, 0)
	for _, r := range rows {
		if _, seen := byHash[r.ContentHash]; !seen {
			order = append(order, r.ContentHash)
		}
		byHash[r.ContentHash] = append(byHash[r.ContentHash], CloneMember{
			NodeID:     r.NodeID,
			SymbolPath: r.SymbolPath,
			FilePath:   r.FilePath,
			Kind:       r.Kind,
			LineStart:  r.LineStart,
			LineEnd:    r.LineEnd,
		})
	}

	groups := make([]CloneGroup, 0, len(order))
	for _, h := range order {
		members := byHash[h]
		if len(members) < 2 {
			continue
		}
		sort.Slice(members, func(i, j int) bool {
			if members[i].FilePath != members[j].FilePath {
				return members[i].FilePath < members[j].FilePath
			}
			return members[i].LineStart < members[j].LineStart
		})
		groups = append(groups, CloneGroup{ContentHash: h, Size: len(members), Members: members})
	}
	sort.Slice(groups, func(i, j int) bool {
		if groups[i].Size != groups[j].Size {
			return groups[i].Size > groups[j].Size
		}
		return groups[i].ContentHash < groups[j].ContentHash
	})
	return groups
}
