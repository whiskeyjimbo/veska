// Package blastradius computes reverse/forward reachability sets over the
// promoted edges table. It is used by the eng_get_blast_radius family of
// MCP tools to answer "what is affected if I change this symbol".
//
// The BFS is intentionally bounded by both max_depth and max_nodes so a
// pathological graph (e.g. a god-object touched by everything) cannot
// stall the MCP handler. When either bound is hit the traversal stops
// and the returned envelope's Truncated flag is set so callers can
// distinguish a complete answer from a clipped one.
package blastradius

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/whiskeyjimbo/veska/internal/application/staging"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// ErrSeedNotFound is returned by Service.Of when none of the supplied seed
// node_ids resolve to a node in (repoID, branch). Handlers should map this to
// a NotFound-style RPC error so callers get a clear signal instead of an
// empty-fields response .
var ErrSeedNotFound = errors.New("blastradius: seed not found")

// ErrMissingDependency is returned by NewService when a required
// dependency is nil. It wraps so callers can errors.Is against it.
var ErrMissingDependency = errors.New("blastradius: missing required dependency")

// Direction selects which adjacency the BFS walks.
//
//   - DirCallers  walks INBOUND edges: "who depends on these seeds".
//     This is the default and matches the SOLO-09 §3 definition of
//     blast radius.
//   - DirCallees  walks OUTBOUND edges: "what do these seeds depend on".
//   - DirBoth     walks both directions on every hop.
type Direction string

const (
	DirCallers Direction = "callers"
	DirCallees Direction = "callees"
	DirBoth    Direction = "both"
)

// ParseDirection maps a tool-layer string to a Direction. An empty string
// returns the default (DirCallers). Unknown values return an error so the
// handler can surface InvalidParams.
func ParseDirection(s string) (Direction, error) {
	switch s {
	case "", string(DirCallers):
		return DirCallers, nil
	case string(DirCallees):
		return DirCallees, nil
	case string(DirBoth):
		return DirBoth, nil
	default:
		return "", fmt.Errorf("unknown direction %q (want callers|callees|both)", s)
	}
}

// Defaults applied by the service when callers pass non-positive bounds.
const (
	DefaultMaxDepth = 3
	DefaultMaxNodes = 200
	HardMaxDepth    = 10
	HardMaxNodes    = 10000

	// DefaultHubDegreeThreshold gates BFS expansion through high-degree
	// "registry" nodes (cobra rootCmd, http muxes, etc.). 50 is a
	// loose-enough cutoff that legitimate fan-out (a popular
	// constructor with 20–40 callers) is unaffected, while framework
	// registry hubs (typically 100+ AddCommand/Handle callers in
	// real cobra/mux apps) get gated. See solov2-l2f5.
	DefaultHubDegreeThreshold = 50
)

// Entry is one node in the radius, tagged with its BFS distance from the
// nearest seed. Distance 0 means "this was a seed".
type Entry struct {
	NodeID     string
	Distance   int
	SymbolPath string
	FilePath   string
	Kind       string
	LineStart  int
	LineEnd    int
	// Snippet is the symbol's stored raw_content (nodes.snippet column),
	// propagated from ports.NodeMeta so downstream consumers like
	// contextpack can return source inline without a separate Read
	// . Empty when the underlying node row has no snippet
	// (legacy rows before the column was added).
	Snippet string
	// IsHub is true when this node's neighbour count exceeded
	// HubDegreeThreshold and BFS skipped expanding through it. The node
	// is still reported (its presence in the blast radius is real) but
	// its further fan-out was suppressed to avoid drowning the result in
	// framework registry noise — see solov2-l2f5.
	IsHub bool `json:"is_hub,omitempty"`
	// Pending is true when the node id was reachable from the BFS but
	// NodeLookup couldn't yet hydrate its metadata — the graph index is
	// eventually-consistent against the edges table, so a freshly-changed
	// file may surface an unresolved node for a few hundred ms. Callers
	// can use this to render "pending" instead of treating the empty
	// name/kind/file_path as a real symbol .
	Pending bool `json:"pending,omitempty"`
}

// Response is the envelope returned by Service.Of and friends.
type Response struct {
	// Entries are ordered by BFS distance, then by the order in which the
	// underlying EdgeReader returned the neighbour. Seeds appear first.
	Entries []Entry
	// Truncated is true when traversal stopped because MaxNodes was hit
	// (rather than naturally exhausting reachable nodes).
	Truncated bool
	// IncludedStaging is true when the in-memory staging area actually
	// contributed seeds to this response (a dirty node was staged via DirtyOf).
	// A clean working tree — or a non-staging path (Of/DiffOf) — reports false,
	// so a consumer can trust the flag as "staging contributed rows"
	// (SOLO-09 4.4), not merely "this is the dirty view" (solov2-nmps.11).
	IncludedStaging bool
}

// Service orchestrates the BFS over EdgeReader and the hydration via
// NodeLookup. It is stateless; the same instance is safe for concurrent
// callers.
type Service struct {
	edges   ports.EdgeReader
	nodes   ports.NodeLookup
	staging *staging.Area
	// defaultHubDegree is the hub-degree threshold applied when a per-call
	// Options leaves HubDegreeThreshold at 0. Seeded to DefaultHubDegreeThreshold
	// by NewService; WithDefaultHubDegreeThreshold overwrites it with any value
	// (including a negative one, which disables the gate). See solov2-l8su.
	defaultHubDegree int
}

// ServiceOption configures a Service at construction.
type ServiceOption func(*Service)

// WithDefaultHubDegreeThreshold overrides the hub-degree threshold used when a
// per-call Options leaves HubDegreeThreshold at 0. Any value is honoured,
// including a negative one (which disables the hub gate) — the daemon threads
// the operator-configured blast.hub_degree_threshold through here.
func WithDefaultHubDegreeThreshold(n int) ServiceOption {
	return func(s *Service) { s.defaultHubDegree = n }
}

// NewService constructs a Service. edges and nodes are required; a nil
// dependency is reported with a wrapped ErrMissingDependency. staging
// may be nil for callers that never invoke DirtyOf.
func NewService(edges ports.EdgeReader, nodes ports.NodeLookup, staging *staging.Area, opts ...ServiceOption) (*Service, error) {
	switch {
	case edges == nil:
		return nil, fmt.Errorf("blastradius.NewService: edges is nil: %w", ErrMissingDependency)
	case nodes == nil:
		return nil, fmt.Errorf("blastradius.NewService: nodes is nil: %w", ErrMissingDependency)
	}
	s := &Service{edges: edges, nodes: nodes, staging: staging, defaultHubDegree: DefaultHubDegreeThreshold}
	for _, o := range opts {
		o(s)
	}
	return s, nil
}

// Options carries the per-call BFS bounds.
type Options struct {
	MaxDepth  int
	MaxNodes  int
	Direction Direction
	// HubDegreeThreshold: nodes whose neighbour count (in the configured
	// direction) exceeds this value act as hubs and are NOT expanded
	// through during BFS. They are still included in the result (with
	// IsHub=true) so callers see the structural fact; what's excluded is
	// the irrelevant fan-out through them.
	//
	// solov2-l2f5 motivation: framework registry nodes like cobra's
	// rootCmd (every command's init() calls rootCmd.AddCommand) become
	// star-shaped hubs. Without this gate, a blast radius from any single
	// command transitively pulls in every other command at distance 2,
	// drowning the real risk signal. Generalises to mux/gin/echo/chi
	// routers and any other "central registry" pattern.
	//
	// 0 (default) → use DefaultHubDegreeThreshold. <0 disables the gate
	// (legacy behaviour: expand through everything).
	HubDegreeThreshold int
}

// applied returns o with zero-valued fields replaced by defaults and
// over-large values clamped to the hard cap. defaultHub is the Service-level
// hub-degree threshold substituted when the caller leaves HubDegreeThreshold
// at 0; a negative defaultHub propagates through to disable the gate.
func (o Options) applied(defaultHub int) Options {
	if o.MaxDepth <= 0 {
		o.MaxDepth = DefaultMaxDepth
	}
	if o.MaxDepth > HardMaxDepth {
		o.MaxDepth = HardMaxDepth
	}
	if o.MaxNodes <= 0 {
		o.MaxNodes = DefaultMaxNodes
	}
	if o.MaxNodes > HardMaxNodes {
		o.MaxNodes = HardMaxNodes
	}
	if o.Direction == "" {
		o.Direction = DirCallers
	}
	if o.HubDegreeThreshold == 0 {
		o.HubDegreeThreshold = defaultHub
	}
	return o
}

// Of runs the BFS from the seed node_ids and returns the hydrated radius.
// Seeds themselves are included in the result at distance 0. Duplicate
// seeds are deduplicated.
func (s *Service) Of(ctx context.Context, repoID, branch string, seedIDs []string, opts Options) (Response, error) {
	opts = opts.applied(s.defaultHubDegree)

	// Seed validation: if NONE of the supplied seed_ids resolve to a real
	// node in (repoID, branch), that's a user error — the radius is
	// undefined, and the historical behaviour of returning a single entry
	// with empty name/kind/file_path silently masked it . A
	// partial miss is still tolerated: downstream BFS entries may be
	// eventually-consistent (the comment further down still applies), but
	// the seed itself is the user's input and we owe them a clear error
	// when ALL inputs are bogus.
	cleanSeeds := make([]string, 0, len(seedIDs))
	for _, id := range seedIDs {
		if id != "" {
			cleanSeeds = append(cleanSeeds, id)
		}
	}
	if len(cleanSeeds) > 0 {
		seedMeta, err := s.nodes.LookupNodes(ctx, repoID, branch, cleanSeeds)
		if err != nil {
			return Response{}, fmt.Errorf("blastradius: seed lookup: %w", err)
		}
		if len(seedMeta) == 0 {
			return Response{}, fmt.Errorf("%w: no seed node_id resolved in repo=%s branch=%s", ErrSeedNotFound, repoID, branch)
		}
	}

	visited := make(map[string]int, opts.MaxNodes)
	order := make([]string, 0, opts.MaxNodes)
	truncated := false

	enqueue := func(id string, dist int) bool {
		if _, seen := visited[id]; seen {
			return true
		}
		if len(order) >= opts.MaxNodes {
			truncated = true
			return false
		}
		visited[id] = dist
		order = append(order, id)
		return true
	}

	// Seed frontier.
	frontier := make([]string, 0, len(seedIDs))
	for _, id := range seedIDs {
		if id == "" {
			continue
		}
		if enqueue(id, 0) {
			frontier = append(frontier, id)
		}
	}

	// hubs records ids whose per-source fan-out exceeded
	// HubDegreeThreshold; the BFS does NOT expand through them, but they
	// still appear in the result with IsHub=true so callers see the fact
	// . Negative threshold disables the gate entirely.
	hubs := make(map[string]bool)
	gateOn := opts.HubDegreeThreshold > 0

	// BFS over the configured direction(s).
	for hop := 0; hop < opts.MaxDepth && len(frontier) > 0 && !truncated; hop++ {
		perSrc, err := s.expandPerSource(ctx, repoID, branch, frontier, opts.Direction)
		if err != nil {
			return Response{}, err
		}
		next := make([]string, 0)
	frontierLoop:
		for _, src := range frontier {
			ns := perSrc[src]
			if gateOn && len(ns) > opts.HubDegreeThreshold {
				hubs[src] = true
				continue
			}
			for _, n := range ns {
				if !enqueue(n, hop+1) {
					break frontierLoop
				}
				next = append(next, n)
			}
		}
		frontier = next
	}

	// Hydrate via NodeLookup. We preserve BFS order in the returned slice.
	hydrated, err := s.nodes.LookupNodes(ctx, repoID, branch, order)
	if err != nil {
		return Response{}, fmt.Errorf("blastradius: node lookup: %w", err)
	}
	byID := make(map[string]ports.NodeMeta, len(hydrated))
	for _, m := range hydrated {
		byID[m.NodeID] = m
	}

	entries := make([]Entry, 0, len(order))
	for _, id := range order {
		m, ok := byID[id]
		// Missing rows still count toward the radius but appear with
		// empty metadata + Pending=true: the index is eventually-
		// consistent vs the authoritative edges + nodes truth, and
		// surfacing the bare ID is more useful than dropping it silently.
		// The flag lets callers distinguish "unresolved-pending" from
		// "real symbol with no name" .
		entries = append(entries, Entry{
			NodeID:     id,
			Distance:   visited[id],
			SymbolPath: m.SymbolPath,
			FilePath:   m.FilePath,
			Kind:       m.Kind,
			LineStart:  m.LineStart,
			LineEnd:    m.LineEnd,
			Snippet:    m.Snippet,
			IsHub:      hubs[id],
			Pending:    !ok,
		})
	}
	return Response{Entries: entries, Truncated: truncated}, nil
}

// expandPerSource is expand's sibling that preserves per-source neighbour
// lists so the BFS caller can gate expansion node-by-node (solov2-l2f5
// hub-degree threshold). Behaviour is otherwise identical to expand: the
// union of in/outbound neighbours per the configured direction.
func (s *Service) expandPerSource(ctx context.Context, repoID, branch string, frontier []string, dir Direction) (map[string][]string, error) {
	out := make(map[string][]string, len(frontier))
	if dir == DirCallers || dir == DirBoth {
		m, err := s.edges.InboundEdges(ctx, repoID, branch, frontier)
		if err != nil {
			return nil, fmt.Errorf("blastradius: inbound: %w", err)
		}
		for _, id := range frontier {
			out[id] = append(out[id], m[id]...)
		}
	}
	if dir == DirCallees || dir == DirBoth {
		m, err := s.edges.OutboundEdges(ctx, repoID, branch, frontier)
		if err != nil {
			return nil, fmt.Errorf("blastradius: outbound: %w", err)
		}
		for _, id := range frontier {
			out[id] = append(out[id], m[id]...)
		}
	}
	return out, nil
}

// ChangedFilesFunc returns the list of files changed against HEAD for the
// given repoRoot. It mirrors the signature of git.ChangedFiles so callers
// can plug the real adapter in while tests pass a deterministic fake.
type ChangedFilesFunc func(ctx context.Context, repoRoot string) ([]string, error)

// ChangedFilesBetweenFunc returns the files that differ between two git
// refs for repoRoot. It mirrors git.ChangedFilesBetween. Callers wanting a
// ranged blast (eng_get_diff_blast_radius with ref_a/ref_b) capture the two
// refs in a closure and pass it to DiffOf as a ChangedFilesFunc — the
// service stays agnostic about how the change set was derived (working-tree
// vs ref range), it just blasts the union of nodes in the changed files.
type ChangedFilesBetweenFunc func(ctx context.Context, repoRoot, refA, refB string) ([]string, error)

// DiffOf computes the blast radius for the union of all nodes whose source
// file appears in the working-tree diff for repoRoot. The change set is
// derived from changedFiles; each file is resolved to its node_ids via
// NodesInFile on the injected NodeLookup; the union forms the seed set
// for Of.
//
// An empty diff returns an empty Response with IncludedStaging=false:
// "no changes" is a valid answer, not a degraded path.
func (s *Service) DiffOf(ctx context.Context, repoID, branch, repoRoot string, changedFiles ChangedFilesFunc, opts Options) (Response, error) {
	if changedFiles == nil {
		return Response{}, fmt.Errorf("blastradius.DiffOf: changedFiles is nil")
	}
	if repoRoot == "" {
		return Response{}, fmt.Errorf("blastradius.DiffOf: repoRoot is empty")
	}
	files, err := changedFiles(ctx, repoRoot)
	if err != nil {
		return Response{}, fmt.Errorf("blastradius: list changed files: %w", err)
	}
	if len(files) == 0 {
		return Response{Entries: []Entry{}}, nil
	}

	seen := make(map[string]struct{})
	seeds := make([]string, 0, len(files)*4)
	for _, fp := range files {
		// git diff and nodes.file_path now both key on the repo-relative slash
		// path (ADR-S0017 §1), so a diff path feeds NodesInFile directly. An
		// absolute path (defensive) is relativised against repoRoot to match.
		if filepath.IsAbs(fp) {
			if rel, rerr := filepath.Rel(repoRoot, fp); rerr == nil {
				fp = filepath.ToSlash(rel)
			}
		}
		ids, err := s.nodes.NodesInFile(ctx, repoID, branch, fp)
		if err != nil {
			return Response{}, fmt.Errorf("blastradius: nodes in %s: %w", fp, err)
		}
		for _, id := range ids {
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			seeds = append(seeds, id)
		}
	}
	if len(seeds) == 0 {
		// Files changed but none of them have promoted nodes. That can
		// happen for new files not yet sealed, or for non-source files.
		return Response{Entries: []Entry{}}, nil
	}
	return s.Of(ctx, repoID, branch, seeds, opts)
}

// ContentHasher is an optional capability that lets DirtyOf compare a
// staged node's parser-computed ContentHash against the promoted side's
// stored hash, filtering out unchanged-but-restaged symbols .
// *sqlite.NodeLookupRepo satisfies it; stubs that don't simply skip the
// filter, preserving the prior "every staged node is dirty" behaviour.
type ContentHasher interface {
	NodeContentHash(ctx context.Context, repoID, branch, nodeID string) (string, error)
}

// DirtyOf runs the BFS from every node currently in the staging overlay
// for (repoID, branch). It is the eng_get_dirty_blast_radius engine: the
// "seed" is the in-flight change set, not a single node_id.
//
// Staged nodes are not themselves in the edges table (edges are written
// only at promotion time), so the BFS expands them via inbound edges
// only — answering "who currently calls things I am about to change".
// The direction option is honoured but the canonical use is callers.
//
// solov2-iyz2: a re-parse stages every symbol in a file regardless of
// whether the symbol body actually changed. To avoid claiming the whole
// file is dirty for a comment-only edit, the seed set is filtered: a
// staged node whose parser-computed ContentHash matches the promoted
// content_hash for the same node_id is unchanged and contributes no
// seed. Filtering requires the optional ContentHasher capability; when
// the lookup adapter doesn't implement it, behaviour falls back to the
// prior "every staged node is a seed" semantics.
func (s *Service) DirtyOf(ctx context.Context, repoID, branch string, opts Options) (Response, error) {
	if s.staging == nil {
		// No staging area wired → staging contributed nothing.
		return Response{IncludedStaging: false}, nil
	}
	hasher, _ := s.nodes.(ContentHasher)
	snap := s.staging.Snapshot(repoID, branch)
	seeds := make([]string, 0, len(snap))
	for _, sf := range snap {
		for _, n := range sf.Nodes {
			if n == nil {
				continue
			}
			if hasher != nil && n.ContentHash != nil {
				promoted, err := hasher.NodeContentHash(ctx, repoID, branch, string(n.ID))
				if err == nil && promoted != "" && promoted == string(*n.ContentHash) {
					// Unchanged symbol — re-parsed because its file was
					// edited, but the body hash matches the promoted
					// version, so it isn't actually dirty.
					continue
				}
			}
			seeds = append(seeds, string(n.ID))
		}
	}
	resp, err := s.Of(ctx, repoID, branch, seeds, opts)
	if err != nil {
		return Response{}, err
	}
	// IncludedStaging is honest about CONTRIBUTION: true only when a dirty node
	// was actually staged (matching the SOLO-09 4.4 contract and the Of/DiffOf
	// paths), not merely because this is the dirty view. A clean working tree
	// yields no seeds and reports false (solov2-nmps.11).
	resp.IncludedStaging = len(seeds) > 0
	return resp, nil
}
