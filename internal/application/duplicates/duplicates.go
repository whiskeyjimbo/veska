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
	"strings"
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
	// StructuralHash is the identifier-normalised (Type-2) hash; set by the
	// structural projection, empty for the exact projection.
	StructuralHash string
	RepoID         string
	NodeID         string
	SymbolPath     string
	FilePath       string
	Kind           string
	LineStart      int
	LineEnd        int
}

// CloneMember is one occurrence of a clone within a CloneGroup.
type CloneMember struct {
	NodeID     string
	RepoID     string
	SymbolPath string
	FilePath   string
	Kind       string
	LineStart  int
	LineEnd    int
	// ContentHash is the member's byte-identity hash, carried so the unified
	// Clusters view can sub-tier a structural group (all-same content_hash =>
	// exact tier; mixed => genuine Type-2). Empty when not hydrated.
	ContentHash string
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
	// StructuralNodes returns every node in (repoID, branch) whose
	// structural_hash is shared by >=2 nodes, excluding excludeKinds — the
	// Type-2 (renamed-variable) clone projection. Returned rows carry the
	// structural_hash in ClonedNode.ContentHash (the grouping key); the Finder
	// folds them the same way as exact clones. NULL structural_hash (nodes the
	// parser did not structurally hash) never groups.
	StructuralNodes(ctx context.Context, repoID, branch string, excludeKinds []string) ([]ClonedNode, error)
}

// DefaultNearThreshold is the near-dup minimum score used for an embedder with
// no measured calibration in calibratedNearThreshold. It equals the model2vec
// value — the default embedder in a normal build — so the common case is
// calibrated even via this fallback.
const DefaultNearThreshold float32 = 0.68

// calibratedNearThreshold maps an elected embedder's ModelID to a measured
// near-dup minimum score (solov2-md3n; harness in tools/loadtest/neardup).
//
// Each value is HIGH-PRECISION by design: set at/just above that embedder's
// "related" (merely-same-domain) score band, so genuinely-distinct functions
// are not surfaced as duplicates — trading recall (the weakest near-dups fall
// below it) for a quiet, low-false-positive findings surface.
//
// Critically, the values are NOT comparable across embedders: similarity
// scores live in per-model spaces ([[embedder-architecture]]). The measurement
// showed near-dup and "related" distributions OVERLAP for every embedder (no
// clean separator), and that one global constant cannot serve all — 0.80 (the
// old provisional) returned almost nothing on model2vec yet leaked unrelated
// pairs on nomic. So the default is selected by elected ModelID; callers
// wanting different recall pass an explicit min_score.
//
// Measured on a small curated corpus with some shared-skeleton contamination —
// treat as first calibration points, not exact gates.
var calibratedNearThreshold = map[string]float32{
	// near-dup median 0.69 vs related max 0.68 — separable at high precision.
	"model2vec(potion-code-16M)": 0.68,
	// scores compress high (near-dup median 0.84, related p90 0.80, unrelated
	// max 0.82); 0.85 clears the related/unrelated bands.
	"nomic-embed-text": 0.85,
	// hash-ngram: bands are INVERTED (near-dup median 0.56 < related max 0.68),
	// so no threshold separates well; 0.70 is high-precision/near-zero-recall.
	// static-v2 is a poor fit for near-dup — prefer model2vec.
	"veska-static-v2": 0.70,
}

// NearThresholdFor returns the calibrated near-dup minimum score for an elected
// embedder ModelID, falling back to DefaultNearThreshold for an unknown or
// empty ID.
//
// Ollama model IDs may carry a ":tag" (e.g. "nomic-embed-text:latest" — the
// ModelID is the verbatim configured model name). A tag pins a version but does
// not change the embedding space, so an exact miss retries on the bare name
// before falling back — otherwise a tagged nomic would silently get the
// model2vec-calibrated default and flood the user.
func NearThresholdFor(modelID string) float32 {
	if v, ok := calibratedNearThreshold[modelID]; ok {
		return v
	}
	if i := strings.IndexByte(modelID, ':'); i > 0 {
		if v, ok := calibratedNearThreshold[modelID[:i]]; ok {
			return v
		}
	}
	return DefaultNearThreshold
}

// SimilarEdge is one persisted SIMILAR_TO edge whose score met the near-dup
// threshold, with both endpoints' metadata already hydrated by the store so the
// Finder can cluster without a second lookup. The edge is treated as undirected
// for clustering — autolink writes a directed top-k edge, but "is a copy of" is
// symmetric.
type SimilarEdge struct {
	Src   CloneMember
	Dst   CloneMember
	Score float32
}

// NearCluster is a connected component of SIMILAR_TO edges: a set of >=2 nodes
// transitively linked above the threshold — N near-identical copies of one
// helper. MinScore/MaxScore bound the edge scores within the component;
// a low MinScore flags a loosely-chained cluster (A~B~C where A and C are only
// transitively similar), which the caller may want to inspect.
type NearCluster struct {
	Size     int
	MinScore float32
	MaxScore float32
	Members  []CloneMember
}

// NearStore is the narrow port the near-dup view needs: every SIMILAR_TO edge
// in (repoID, branch) whose score >= minScore and whose BOTH endpoints are
// outside excludeKinds, with endpoint metadata hydrated. Reads only what
// autolink already persisted — no new similarity sweep. Edges with a NULL
// score (legacy rows promoted before the score column existed) are omitted.
type NearStore interface {
	SimilarEdges(ctx context.Context, repoID, branch string, minScore float32, excludeKinds []string) ([]SimilarEdge, error)
}

// ErrMissingDependency is returned by NewFinder when a required collaborator is
// nil. errors.Is-matchable so callers distinguish a wiring fault from a runtime
// failure, mirroring the autolink / promoter constructors.
var ErrMissingDependency = errors.New("duplicates: missing required dependency")

// Finder computes duplicate groups: exact clones (content_hash) and
// near-duplicate clusters (thresholded SIMILAR_TO edges). The zero value is not
// usable; construct with NewFinder.
type Finder struct {
	clones     CloneStore
	near       NearStore
	embedderID string // elected embedder ModelID; selects the calibrated near-dup default
}

// NewFinder constructs a Finder. Both stores are required: a nil dependency
// yields an error wrapping ErrMissingDependency and a nil *Finder. embedderID
// is the elected embedder's ModelID (e.g. "model2vec(potion-code-16M)"); it
// selects the calibrated near-dup default via NearThresholdFor. An empty ID
// falls back to DefaultNearThreshold.
func NewFinder(clones CloneStore, near NearStore, embedderID string) (*Finder, error) {
	if clones == nil {
		return nil, fmt.Errorf("duplicates.NewFinder: clone store is nil: %w", ErrMissingDependency)
	}
	if near == nil {
		return nil, fmt.Errorf("duplicates.NewFinder: near store is nil: %w", ErrMissingDependency)
	}
	return &Finder{clones: clones, near: near, embedderID: embedderID}, nil
}

// ExactClones returns the content_hash clone groups in (repoID, branch),
// excluding container/sub-symbol kinds. Every returned group has Size >= 2.
//
// Ordering is deterministic: groups by descending Size then ascending
// ContentHash (most-copied first, stable tie-break); members within a group by
// (FilePath, LineStart) so the same physical layout always renders the same.
func (f *Finder) ExactClones(ctx context.Context, repoID, branch string) ([]CloneGroup, error) {
	rows, err := f.clones.ClonedNodes(ctx, repoID, branch, ExcludedKinds)
	if err != nil {
		return nil, fmt.Errorf("duplicates.ExactClones: %w", err)
	}
	return groupByHash(rows, func(r ClonedNode) string { return r.ContentHash }), nil
}

// StructuralClones returns Type-2 clone groups in (repoID, branch): sets of >=2
// nodes sharing a structural_hash (identical shape after a consistent rename),
// excluding container/sub-symbol kinds. Every group has Size >= 2. A group
// whose members also all share one content_hash is a pure exact clone (the
// unified Clusters view promotes those to the exact tier); on its own this
// surface returns every structurally-identical group, exact or renamed.
//
// Ordering matches ExactClones: groups by descending Size then ascending hash;
// members by (FilePath, LineStart).
func (f *Finder) StructuralClones(ctx context.Context, repoID, branch string) ([]CloneGroup, error) {
	rows, err := f.clones.StructuralNodes(ctx, repoID, branch, ExcludedKinds)
	if err != nil {
		return nil, fmt.Errorf("duplicates.StructuralClones: %w", err)
	}
	return groupByHash(rows, func(r ClonedNode) string { return r.StructuralHash }), nil
}

// NearDuplicates returns near-identical clusters: connected components of
// SIMILAR_TO edges whose score >= minScore. A non-positive minScore falls back
// to the calibrated default for this Finder's elected embedder
// (NearThresholdFor). Every returned cluster has Size >= 2 (each edge
// contributes two members). It reads only edges autolink already persisted —
// no new similarity sweep.
//
// Ordering is deterministic: clusters by descending Size, then descending
// MinScore (tightest first among same-size), then by the first member's NodeID;
// members within a cluster by (FilePath, LineStart).
func (f *Finder) NearDuplicates(ctx context.Context, repoID, branch string, minScore float32) ([]NearCluster, error) {
	if minScore <= 0 {
		minScore = NearThresholdFor(f.embedderID)
	}
	edges, err := f.near.SimilarEdges(ctx, repoID, branch, minScore, ExcludedKinds)
	if err != nil {
		return nil, fmt.Errorf("duplicates.NearDuplicates: %w", err)
	}
	return clusterEdges(edges), nil
}

// groupByHash folds flat ClonedNode rows into deterministically-ordered
// CloneGroups, dropping any hash that ended up with a single member (defensive:
// the store already enforces COUNT>=2, but grouping here keeps the invariant
// local and lets the store stay a dumb projection).
func groupByHash(rows []ClonedNode, keyOf func(ClonedNode) string) []CloneGroup {
	byHash := make(map[string][]CloneMember)
	order := make([]string, 0)
	for _, r := range rows {
		k := keyOf(r)
		if _, seen := byHash[k]; !seen {
			order = append(order, k)
		}
		byHash[k] = append(byHash[k], CloneMember{
			NodeID:      r.NodeID,
			RepoID:      r.RepoID,
			SymbolPath:  r.SymbolPath,
			FilePath:    r.FilePath,
			Kind:        r.Kind,
			LineStart:   r.LineStart,
			LineEnd:     r.LineEnd,
			ContentHash: r.ContentHash,
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

// component accumulates the member set and score bounds of one connected
// component during clustering.
type component struct {
	ids                []string
	minScore, maxScore float32
}

// clusterEdges folds undirected SIMILAR_TO edges into connected components via
// union-find, then builds a deterministically-ordered NearCluster per
// component with its member set, min/max edge score, and size.
func clusterEdges(edges []SimilarEdge) []NearCluster {
	uf := newUnionFind()
	members := make(map[string]CloneMember)
	for _, e := range edges {
		members[e.Src.NodeID] = e.Src
		members[e.Dst.NodeID] = e.Dst
		uf.union(e.Src.NodeID, e.Dst.NodeID)
	}
	return buildClusters(aggregateComponents(uf, edges), members)
}

// aggregateComponents walks the edges once, attributing each edge's score
// bounds and endpoints to its component root.
func aggregateComponents(uf *unionFind, edges []SimilarEdge) map[string]*component {
	byRoot := make(map[string]*component)
	seen := make(map[string]bool)
	for _, e := range edges {
		c := byRoot[uf.find(e.Src.NodeID)]
		if c == nil {
			c = &component{minScore: e.Score, maxScore: e.Score}
			byRoot[uf.find(e.Src.NodeID)] = c
		}
		if e.Score < c.minScore {
			c.minScore = e.Score
		}
		if e.Score > c.maxScore {
			c.maxScore = e.Score
		}
		for _, id := range []string{e.Src.NodeID, e.Dst.NodeID} {
			if !seen[id] {
				seen[id] = true
				c.ids = append(c.ids, id)
			}
		}
	}
	return byRoot
}

// buildClusters hydrates components into NearClusters with sorted members, then
// orders the clusters by descending size, then descending MinScore, then the
// first member's NodeID.
func buildClusters(byRoot map[string]*component, members map[string]CloneMember) []NearCluster {
	clusters := make([]NearCluster, 0, len(byRoot))
	for _, c := range byRoot {
		cm := make([]CloneMember, 0, len(c.ids))
		for _, id := range c.ids {
			cm = append(cm, members[id])
		}
		sort.Slice(cm, func(i, j int) bool {
			if cm[i].FilePath != cm[j].FilePath {
				return cm[i].FilePath < cm[j].FilePath
			}
			return cm[i].LineStart < cm[j].LineStart
		})
		clusters = append(clusters, NearCluster{Size: len(cm), MinScore: c.minScore, MaxScore: c.maxScore, Members: cm})
	}
	sort.Slice(clusters, func(i, j int) bool {
		if clusters[i].Size != clusters[j].Size {
			return clusters[i].Size > clusters[j].Size
		}
		if clusters[i].MinScore != clusters[j].MinScore {
			return clusters[i].MinScore > clusters[j].MinScore
		}
		return clusters[i].Members[0].NodeID < clusters[j].Members[0].NodeID
	})
	return clusters
}

// unionFind is a tiny disjoint-set with path compression, scoped to one
// NearDuplicates call (node IDs are the elements).
type unionFind struct {
	parent map[string]string
}

func newUnionFind() *unionFind { return &unionFind{parent: make(map[string]string)} }

func (u *unionFind) find(x string) string {
	p, ok := u.parent[x]
	if !ok {
		u.parent[x] = x
		return x
	}
	if p != x {
		u.parent[x] = u.find(p)
	}
	return u.parent[x]
}

func (u *unionFind) union(a, b string) {
	ra, rb := u.find(a), u.find(b)
	if ra != rb {
		u.parent[ra] = rb
	}
}
