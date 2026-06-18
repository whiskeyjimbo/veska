// Package duplicates finds exact-clone groups of nodes (functions/symbols) with
// byte-identical content hashes. Near-duplicate detection is deferred to vector
// similarity search (`veska similar`), avoiding a global clustering scan.
package duplicates

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// ExcludedKinds blocks structural container or trivial node types that do not
// carry a refactoring signal, maintaining parity with autolink's symbol filters.
var ExcludedKinds = []string{"package", "chunk", "file", "module", "field", "import"}

type ClonedNode struct {
	ContentHash string
	// StructuralHash is empty for exact clone sweeps and set for structural Type-2 sweeps.
	StructuralHash string
	RepoID         string
	NodeID         string
	SymbolPath     string
	FilePath       string
	Kind           string
	LineStart      int
	LineEnd        int
}

type CloneMember struct {
	NodeID     string
	RepoID     string
	SymbolPath string
	FilePath   string
	Kind       string
	LineStart  int
	LineEnd    int
	// ContentHash sub-tiers structural groups to identify exact clones within Type-2 structural groups.
	ContentHash string
}

type CloneGroup struct {
	ContentHash string
	Size        int
	Members     []CloneMember
}

// CloneStore defines the storage port for identifying duplicate nodes in a given branch.
type CloneStore interface {
	ClonedNodes(ctx context.Context, q CloneQuery, excludeKinds []string) ([]ClonedNode, error)
	// StructuralNodes returns renamed Type-2 clone matches, ignoring nodes the parser failed to structurally hash.
	StructuralNodes(ctx context.Context, q CloneQuery, excludeKinds []string) ([]ClonedNode, error)
}

// CloneQuery limits the duplicate search scope; an empty RepoID scans all repositories.
type CloneQuery struct {
	RepoID     string
	Branch     string
	PathPrefix string
}

// DefaultNearThreshold matches model2vec's calibrated limit as the default embedder.
const DefaultNearThreshold float32 = 0.68

// calibratedNearThreshold maps embedder models to similarity score limits calibrated
// to minimize false positives. Scores are model-specific and not comparable across architectures.
var calibratedNearThreshold = map[string]float32{
	// near-dup median 0.69 vs related max 0.68 - separable at high precision.
	"model2vec(potion-code-16M)": 0.68,
	// scores compress high (near-dup median 0.84, related p90 0.80, unrelated
	// max 0.82); 0.85 clears the related/unrelated bands.
	"nomic-embed-text": 0.85,
	// hash-ngram: bands are INVERTED (near-dup median 0.56 < related max 0.68),
	// so no threshold separates well; 0.70 is high-precision/near-zero-recall.
	// static-v2 is a poor fit for near-dup - prefer model2vec.
	"veska-static-v2": 0.70,
}

// NearThresholdFor parses Ollama model IDs to discard tags, returning the base
// model's calibration to prevent score misalignments.
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

// SimilarEdge represents a thresholded similarity relationship between two hydrated
// nodes, treated as undirected during clustering.
type SimilarEdge struct {
	Src   CloneMember
	Dst   CloneMember
	Score float32
}

// NearCluster groups transitively similar nodes; a low MinScore indicates looser
// similarity chaining.
type NearCluster struct {
	Size     int
	MinScore float32
	MaxScore float32
	Members  []CloneMember
}

// NearStore retrieves persisted similar-to relationships, ignoring legacy rows
// lacking similarity scores.
type NearStore interface {
	SimilarEdges(ctx context.Context, repoID, branch string, minScore float32, excludeKinds []string) ([]SimilarEdge, error)
}

// ErrMissingDependency is returned by NewFinder when a required collaborator is
// nil. errors.Is-matchable so callers distinguish a wiring fault from a runtime
// failure, mirroring the autolink / promoter constructors.
var ErrMissingDependency = errors.New("duplicates: missing required dependency")

type Finder struct {
	clones     CloneStore
	near       NearStore
	embedderID string // elected embedder ModelID; selects the calibrated near-dup default
}

// NewFinder initializes a Finder, selecting the calibrated threshold for the elected embedder.
func NewFinder(clones CloneStore, near NearStore, embedderID string) (*Finder, error) {
	if clones == nil {
		return nil, fmt.Errorf("duplicates.NewFinder: clone store is nil: %w", ErrMissingDependency)
	}
	if near == nil {
		return nil, fmt.Errorf("duplicates.NewFinder: near store is nil: %w", ErrMissingDependency)
	}
	return &Finder{clones: clones, near: near, embedderID: embedderID}, nil
}

// ExactClones returns duplicate groups with stable, deterministic sorting by group size
// and symbol locations.
func (f *Finder) ExactClones(ctx context.Context, repoID, branch string) ([]CloneGroup, error) {
	rows, err := f.clones.ClonedNodes(ctx, CloneQuery{RepoID: repoID, Branch: branch}, ExcludedKinds)
	if err != nil {
		return nil, fmt.Errorf("duplicates.ExactClones: %w", err)
	}
	return groupByHash(rows, func(r ClonedNode) string { return r.ContentHash }), nil
}

// StructuralClones returns Type-2 renamed groups, including any that are also exact clones.
func (f *Finder) StructuralClones(ctx context.Context, repoID, branch string) ([]CloneGroup, error) {
	rows, err := f.clones.StructuralNodes(ctx, CloneQuery{RepoID: repoID, Branch: branch}, ExcludedKinds)
	if err != nil {
		return nil, fmt.Errorf("duplicates.StructuralClones: %w", err)
	}
	return groupByHash(rows, func(r ClonedNode) string { return r.StructuralHash }), nil
}

// NearDuplicates groups similar nodes into connected components using existing similarity edges.
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

// groupByHash clusters flat node rows into groups, dropping single-member groups to safeguard
// the store's uniqueness constraint.
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

type component struct {
	ids                []string
	minScore, maxScore float32
}

// clusterEdges groups similarity relationships into transitively connected components via union-find.
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

// aggregateComponents aggregates edge score bounds to each component's root node.
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

// buildClusters transforms disjoint components into sorted NearCluster models.
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

// unionFind implements a path-compressed disjoint-set to group node IDs within a query sweep.
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
