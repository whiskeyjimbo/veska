// SPDX-License-Identifier: AGPL-3.0-only

package autolink

import (
	"context"
	"fmt"
	"strings"

	"github.com/whiskeyjimbo/veska/internal/application/pathfilter"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// candidateProducer is the minimal contract the Handler needs from the
// Linker so the handler can be tested without spinning up the embedding
// + vector stack. *Linker satisfies this interface structurally.
type candidateProducer interface {
	Candidates(ctx context.Context, repoID, branch string, sourceNodeIDs []string) ([]Candidate, error)
}

// fileNodeLookup is the narrow port the Handler needs from ports.NodeLookup.
// Defined here so the autolink package does not import the full NodeLookup
// surface (which carries the LookupNodes method aimed at the search layer).
// NodeContentHash returns nodes.content_hash for a single node scoped to
// (repoID, branch). An unknown node MUST return ("", nil) - the Handler
// treats a missing source as "no hash recorded" (NULL on the finding) rather
// than as an error, mirroring the eventual-consistency contract on LookupNodes.
// NOTE: this is the SOURCE node's content_hash (the dirty side that re-ran
// auto-link); it is intentionally distinct from the embedding-input hash on
// node_embedding_refs.content_hash, which is keyed by ports.EmbeddingRefRepo.
type fileNodeLookup interface {
	NodesInFile(ctx context.Context, repoID, branch, filePath string) ([]string, error)
	NodeContentHash(ctx context.Context, repoID, branch, nodeID string) (string, error)
	// LookupNodes hydrates node IDs into their minimal metadata, scoped to
	// (repoID, branch). Used to (a) drop non-symbol container nodes from the
	// auto-link source set and (b) render the target by name/path in the
	// finding message instead of an opaque node ID. IDs absent
	// from storage are omitted, mirroring ports.NodeLookup.
	LookupNodes(ctx context.Context, repoID, branch string, nodeIDs []string) ([]ports.NodeMeta, error)
}

// embedReadiness reports how many of a file's nodes still have a pending
// embedding. The Handler uses it to defer a row whose file is not fully
// embedded yet, rather than running the vector search against a partial store
// and silently under-linking the not-yet-embedded sources. Kept narrow so the
// autolink package depends only on the one method it needs; sqlite's
// EmbeddingRefsRepo satisfies it structurally.
type embedReadiness interface {
	PendingAmong(ctx context.Context, nodeIDs []string) (int, error)
}

// nonSymbolKinds are container / sub-symbol node kinds for which a
// nearest-neighbor "similar to" link is noise: package and chunk nodes embed
// near-identical boilerplate across files and flood the findings list
// A blocklist (rather than a symbol allowlist) keeps unknown or
// future symbol kinds eligible by default.
var nonSymbolKinds = map[string]bool{
	"package": true,
	"chunk":   true,
	"file":    true,
	"module":  true,
	"field":   true,
	"import":  true,
}

// Handler implements queue.WorkHandler for WorkKindAutoLink rows.
// One Row -> one batch of unresolved Edges + one Finding per Edge:
//  1. Validate row.Kind == WorkKindAutoLink.
//  2. Resolve the payload file path to its set of source node_ids.
//  3. Ask the Linker for top-k similarity candidates across those sources.
//  4. Persist each candidate as a SIMILAR_TO edge with Confidence=Unresolved.
//  5. Persist one source_layer='semantic' Finding per candidate, anchored
//     on the edge_id (stored in the findings.node_id TEXT column, which is
//     intentionally schemaless wrt foreign keys at the SQL level).
//
// Idempotency: EdgeStorage uses ON CONFLICT DO NOTHING; FindingStorage uses
// ON CONFLICT DO UPDATE on a finding_id derived from (rule + anchor). Both
// paths handle re-delivery from the queue.Poller without duplication.
type Handler struct {
	linker   candidateProducer
	lookup   fileNodeLookup
	edges    ports.EdgeStorage
	findings ports.FindingStorage

	// repoKind, when set, returns the registered Kind ("tracked" /
	// "ephemeral") of a repo. ephemeral repos (search --repo <url>
	// clones) skip autolink entirely - a 75-file external clone like
	// spf13/pflag otherwise yields ~100 low-severity findings on the
	// junior's first 'findings list'. When the option
	// is unset (older composition roots), behavior is unchanged.
	repoKind func(ctx context.Context, repoID string) (string, error)

	// readiness, when set, gates each row on its file being fully embedded:
	// the handler returns ports.ErrDeferWork while any of the file's nodes are
	// still pending, so the poller retries the row later instead of linking
	// against a partial store. Unset leaves the handler running immediately,
	// relying on an external lane-level gate for correctness.
	readiness embedReadiness
}

// HandlerOption configures a Handler. None are required today; the type is
// here so future cross-cutting concerns (metrics, clocks) can land without a
// breaking constructor change.
type HandlerOption func(*Handler)

// WithRepoKindLookup wires a callback that returns a repo's Kind
// ("tracked" / "ephemeral"). Used by Handle to skip autolink on
// ephemeral repos so externally-cloned codebases don't produce a wall
// of noise findings.
func WithRepoKindLookup(fn func(ctx context.Context, repoID string) (string, error)) HandlerOption {
	return func(h *Handler) { h.repoKind = fn }
}

// WithEmbedReadiness wires the per-file embed gate. With it set, Handle defers
// a row (ports.ErrDeferWork) while any of the file's nodes are still pending an
// embedding, so autolink runs each file against a vector store that already
// holds that file's own sources. Without it, the handler relies on a
// lane-level gate to hold autolink until the embed backlog drains.
func WithEmbedReadiness(r embedReadiness) HandlerOption {
	return func(h *Handler) { h.readiness = r }
}

// NewHandler constructs a Handler. All four collaborators are required; a nil
// argument yields an error wrapping ErrMissingDependency and a nil *Handler,
// mirroring the NewLinker contract in this package.
func NewHandler(
	linker candidateProducer,
	lookup fileNodeLookup,
	edges ports.EdgeStorage,
	findings ports.FindingStorage,
	opts ...HandlerOption,
) (*Handler, error) {
	if linker == nil {
		return nil, fmt.Errorf("autolink.NewHandler: linker is nil: %w", ErrMissingDependency)
	}
	if lookup == nil {
		return nil, fmt.Errorf("autolink.NewHandler: lookup is nil: %w", ErrMissingDependency)
	}
	if edges == nil {
		return nil, fmt.Errorf("autolink.NewHandler: edges is nil: %w", ErrMissingDependency)
	}
	if findings == nil {
		return nil, fmt.Errorf("autolink.NewHandler: findings is nil: %w", ErrMissingDependency)
	}
	h := &Handler{
		linker:   linker,
		lookup:   lookup,
		edges:    edges,
		findings: findings,
	}
	for _, o := range opts {
		o(h)
	}
	return h, nil
}

// Rule is the finding rule emitted by the auto-link handler. Exposed so
// tests and other tooling (suppressions, dashboards) can reference it
// without re-hard-coding the string.
const Rule = "auto-link"

// Handle processes a single ports.WorkRow of kind WorkKindAutoLink.
// Behavior:
//
//	Wrong kind returns an error (programmer or routing bug).
//	Empty payload returns nil (nothing to do).
//	File with zero nodes is a no-op.
//	Linker / EdgeStorage / FindingStorage errors propagate wrapped so the
//	  queue.Poller can re-queue or mark the row failed.
func (h *Handler) Handle(ctx context.Context, row ports.WorkRow) error {
	if row.Kind != ports.WorkKindAutoLink {
		return fmt.Errorf("autolink.Handle: unexpected kind %q", row.Kind)
	}
	filePath := row.Payload
	if filePath == "" {
		return nil
	}
	// ephemeral repos (cache-tier clones from
	// `veska search --repo <url>`) skip autolink entirely. The user is
	// exploring an external codebase, not curating its findings; emitting
	// N×N "similar to" findings on a 75-file pflag clone trains them to
	// ignore the findings surface from day one.
	if h.repoKind != nil {
		if kind, err := h.repoKind(ctx, row.RepoID); err == nil && kind == "ephemeral" {
			return nil
		}
	}
	// Vendored / third-party files are skipped wholesale: proposing
	// auto-link edges from cobra internals or node_modules produces pure
	// noise on a junior's first promotion. The same path
	// predicate gates the dead-code and secret_leak rules.
	if pathfilter.IsVendored(filePath) {
		return nil
	}

	nodeIDs, err := h.lookup.NodesInFile(ctx, row.RepoID, row.Branch, filePath)
	if err != nil {
		return fmt.Errorf("autolink.Handle: nodes in file %q: %w", filePath, err)
	}
	if len(nodeIDs) == 0 {
		return nil
	}

	// Defer the row while this file is still embedding. Autolink links a node by
	// searching the vector store for its neighbors; a node whose own embedding
	// is pending would be skipped with no retry, and a file that runs before its
	// neighbors are embedded links against a partial store. Gating per file lets
	// autolink overlap the embed drain while each file still sees its own
	// sources. Checked before the metadata hydration below so a deferred row
	// costs one COUNT, not a full lookup, each retry.
	if h.readiness != nil {
		pending, err := h.readiness.PendingAmong(ctx, nodeIDs)
		if err != nil {
			return fmt.Errorf("autolink.Handle: embed readiness for %q: %w", filePath, err)
		}
		if pending > 0 {
			return ports.ErrDeferWork
		}
	}

	sources, srcMeta, err := h.resolveSources(ctx, row, nodeIDs)
	if err != nil {
		return err
	}
	if len(sources) == 0 {
		return nil
	}

	// Supersede any prior open auto-link findings whose source node is in this
	// file: re-promoting a file can yield a different set of nearest-neighbor
	// targets (embeddings drift, vector backend tie-breaks reorder), so older
	// (now-orphaned) auto-link findings would otherwise accumulate alongside
	// the fresh ones and the "open findings" surface would balloon across
	// reindexes. Auto-link findings anchor on edge_id, not
	// node_id, so the revalidation sweep cannot reach them - the supersession
	// has to happen at write time.
	// The close runs BEFORE the linker call so the new finding-Save's
	// ON CONFLICT path correctly re-opens (closed_at NULL, state='open') any
	// finding that survived from the previous round on the same edge.
	if err := h.findings.CloseSupersededAutoLinks(ctx, row.RepoID, row.Branch, sources); err != nil {
		return fmt.Errorf("autolink.Handle: close superseded findings: %w", err)
	}

	cands, err := h.linker.Candidates(ctx, row.RepoID, row.Branch, sources)
	if err != nil {
		return fmt.Errorf("autolink.Handle: linker: %w", err)
	}
	if len(cands) == 0 {
		return nil
	}

	cands, tgtMeta, err := h.filterCandidates(ctx, row, cands, srcMeta)
	if err != nil {
		return err
	}
	if len(cands) == 0 {
		return nil
	}

	return h.emitFindings(ctx, row, cands, srcMeta, tgtMeta, nodeIDs)
}

// resolveSources hydrates the file's node IDs and drops container/sub-symbol
// source nodes (package, chunk, …): linking them is noise. Nodes whose
// metadata is missing (index lag) are kept - this is best-effort discovery,
// not a correctness invariant. It returns the eligible source node IDs and the
// full source metadata (reused downstream for filtering and finding labels).
func (h *Handler) resolveSources(ctx context.Context, row ports.WorkRow, nodeIDs []string) ([]string, []ports.NodeMeta, error) {
	srcMeta, err := h.lookup.LookupNodes(ctx, row.RepoID, row.Branch, nodeIDs)
	if err != nil {
		return nil, nil, fmt.Errorf("autolink.Handle: lookup source nodes: %w", err)
	}
	kindByID := make(map[string]string, len(srcMeta))
	for _, m := range srcMeta {
		kindByID[m.NodeID] = m.Kind
	}
	sources := make([]string, 0, len(nodeIDs))
	for _, id := range nodeIDs {
		if nonSymbolKinds[kindByID[id]] {
			continue
		}
		sources = append(sources, id)
	}
	return sources, srcMeta, nil
}

// filterCandidates hydrates the candidate targets and drops the noise pairs:
// targets that are container/sub-symbol kinds, targets that live in the same
// file as their source, and idiomatic-name matches.
// Without these filters a tiny repo immediately gets a noise finding like
// "Similar to chunk:1-22 in main.go" - useless to the user and leaks the
// internal chunk artifact name. Filtering at the candidate level (after the
// linker call) keeps the linker's vector-space logic generic while ensuring
// the user-visible side is clean. The returned target metadata is reused for
// the finding labels.
func (h *Handler) filterCandidates(ctx context.Context, row ports.WorkRow, cands []Candidate, srcMeta []ports.NodeMeta) ([]Candidate, []ports.NodeMeta, error) {
	targetIDs := make([]string, 0, len(cands))
	for _, c := range cands {
		targetIDs = append(targetIDs, c.TargetNodeID)
	}
	tgtMeta, err := h.lookup.LookupNodes(ctx, row.RepoID, row.Branch, targetIDs)
	if err != nil {
		return nil, nil, fmt.Errorf("autolink.Handle: lookup target nodes: %w", err)
	}

	srcKindByID := make(map[string]string, len(srcMeta))
	srcFileByID := make(map[string]string, len(srcMeta))
	srcSymByID := make(map[string]string, len(srcMeta))
	for _, m := range srcMeta {
		srcKindByID[m.NodeID] = m.Kind
		srcFileByID[m.NodeID] = m.FilePath
		srcSymByID[m.NodeID] = m.SymbolPath
	}
	tgtKindByID := make(map[string]string, len(tgtMeta))
	tgtFileByID := make(map[string]string, len(tgtMeta))
	tgtSymByID := make(map[string]string, len(tgtMeta))
	for _, m := range tgtMeta {
		tgtKindByID[m.NodeID] = m.Kind
		tgtFileByID[m.NodeID] = m.FilePath
		tgtSymByID[m.NodeID] = m.SymbolPath
	}

	filtered := cands[:0]
	for _, c := range cands {
		if nonSymbolKinds[tgtKindByID[c.TargetNodeID]] {
			continue
		}
		if tgtFileByID[c.TargetNodeID] != "" && tgtFileByID[c.TargetNodeID] == srcFileByID[c.SourceNodeID] {
			continue
		}
		if isIdiomaticAutolinkNoise(srcSymByID[c.SourceNodeID], tgtSymByID[c.TargetNodeID], srcKindByID[c.SourceNodeID], tgtKindByID[c.TargetNodeID]) {
			continue
		}
		filtered = append(filtered, c)
	}
	return filtered, tgtMeta, nil
}

// emitFindings persists each surviving candidate as a SIMILAR_TO edge and emits
// one source_layer='semantic' Finding per edge. cands and the edges it builds
// are parallel slices; the finding loop walks them by index.: the
// message names BOTH sides - "X in src.go similar to Y in tgt.go" - falling
// back to the opaque node ID when a side's metadata is missing.
func (h *Handler) emitFindings(ctx context.Context, row ports.WorkRow, cands []Candidate, srcMeta, tgtMeta []ports.NodeMeta, nodeIDs []string) error {
	edges := make([]*domain.Edge, 0, len(cands))
	for _, c := range cands {
		e, err := domain.NewEdge(
			domain.EdgeSpec{
				Src:  domain.NodeID(c.SourceNodeID),
				Tgt:  domain.NodeID(c.TargetNodeID),
				Kind: domain.EdgeSimilarTo,
			},
			domain.WithConfidence(domain.Unresolved),
			// Persist the similarity score so near-duplicate detection can
			// threshold SIMILAR_TO edges above autolink's related cutoff
			// Same score space as the finding message.
			domain.WithScore(c.Score),
		)
		if err != nil {
			return fmt.Errorf("autolink.Handle: build edge: %w", err)
		}
		edges = append(edges, e)
	}

	if err := h.edges.SaveEdges(ctx, row.RepoID, row.Branch, edges); err != nil {
		return fmt.Errorf("autolink.Handle: save edges: %w", err)
	}

	srcFileByID := make(map[string]string, len(srcMeta))
	srcDisplayByID := make(map[string]string, len(srcMeta))
	for _, m := range srcMeta {
		srcFileByID[m.NodeID] = m.FilePath
		srcDisplayByID[m.NodeID] = m.SymbolPath + " in " + m.FilePath
	}
	// Build display labels for the (already-hydrated) target metadata, so the
	// finding names the symbol+file rather than an opaque node ID.
	displayByID := make(map[string]string, len(tgtMeta))
	for _, m := range tgtMeta {
		displayByID[m.NodeID] = m.SymbolPath + " in " + m.FilePath
	}

	// Cache source-node content hashes across the candidate set so a handful
	// of source nodes per file do not turn into N look-ups when k >> 1.
	hashCache := make(map[string]string, len(nodeIDs))
	built := make([]*domain.Finding, 0, len(cands))
	for i, c := range cands {
		e := edges[i]
		hash, ok := hashCache[c.SourceNodeID]
		if !ok {
			h2, err := h.lookup.NodeContentHash(ctx, row.RepoID, row.Branch, c.SourceNodeID)
			if err != nil {
				return fmt.Errorf("autolink.Handle: node content hash %q: %w", c.SourceNodeID, err)
			}
			hash = h2
			hashCache[c.SourceNodeID] = hash
		}

		// Anchor the finding on the edge_id (opaque TEXT in findings.node_id).
		// This makes (rule, anchor) unique per candidate edge, so finding_id
		// is unique per candidate and the ON CONFLICT(finding_id, branch)
		// idempotency in FindingRepo applies cleanly on re-delivery.
		// The captured content_hash is the SOURCE node's hash so the
		// revalidation sweep can supersede this finding once the source
		// drifts (the target side is observed via the edge resolver path).
		opts := []domain.FindingOption{domain.WithNodeAnchor(e.ID)}
		if hash != "" {
			opts = append(opts, domain.WithAnchorContentHash(hash))
		}
		// surface the source node's file path so `veska
		// findings list` populates the FILE column. The edge_id anchor in
		// NodeID is opaque to users; the file_path lets findings be
		// scanned/grouped by file like vulnerable_dependency rows already
		// are. NodeAnchor still drives finding_id derivation.
		if srcFile := srcFileByID[c.SourceNodeID]; srcFile != "" {
			opts = append(opts, domain.WithFileAnchor(srcFile))
		}
		target := displayByID[c.TargetNodeID]
		if target == "" {
			target = c.TargetNodeID
		}
		src := srcDisplayByID[c.SourceNodeID]
		if src == "" {
			src = c.SourceNodeID
		}
		f, err := domain.NewFinding(domain.FindingSpec{
			RepoID:   row.RepoID,
			Branch:   row.Branch,
			Severity: domain.SeverityLow,
			Layer:    domain.LayerSemantic,
			Rule:     Rule,
			Message:  fmt.Sprintf("%s similar to %s (score %.2f)", src, target, c.Score),
		}, opts...)
		if err != nil {
			return fmt.Errorf("autolink.Handle: build finding: %w", err)
		}
		built = append(built, f)
	}

	return h.saveFindings(ctx, built)
}

// batchFindingSaver is the optional fast path: a FindingStorage that can upsert
// a whole slice in one transaction. sqlite.FindingRepo implements it; the
// per-row Save fallback keeps test fakes and other stores working unchanged.
type batchFindingSaver interface {
	SaveBatch(ctx context.Context, findings []*domain.Finding) error
}

// saveFindings writes a file's findings in one transaction when the storage
// supports it, else falls back to per-finding Save. On a cold scan a file emits
// up to k findings per source node; the batch path collapses thousands of
// per-row WAL commits into one per file.
func (h *Handler) saveFindings(ctx context.Context, findings []*domain.Finding) error {
	if len(findings) == 0 {
		return nil
	}
	if bs, ok := h.findings.(batchFindingSaver); ok {
		if err := bs.SaveBatch(ctx, findings); err != nil {
			return fmt.Errorf("autolink.Handle: save findings: %w", err)
		}
		return nil
	}
	for _, f := range findings {
		if err := h.findings.Save(ctx, f); err != nil {
			return fmt.Errorf("autolink.Handle: save finding: %w", err)
		}
	}
	return nil
}

// Compile-time check that *Handler satisfies ports.WorkHandler (and, by
// type alias, the historical infrastructure/sqlite/queue.WorkHandler).
var _ ports.WorkHandler = (*Handler)(nil)

// idiomaticIdenticalNames is the set of unqualified symbol names where
// "src and tgt share the same name" is by-construction true across files
// and carries no signal: every Go package has its own init, every
// runnable program has main, Stringer-conforming types all define
// String, error-bearing types all define Error, etc. Auto-link
// candidates that match name-on-name in this set are dropped before the
// findings emit.
var idiomaticIdenticalNames = map[string]struct{}{
	"init":     {},
	"main":     {},
	"String":   {},
	"Error":    {},
	"TestMain": {},
}

// isIdiomaticAutolinkNoise reports whether a (src, tgt) auto-link
// candidate is structurally trivial. Today's rules:
//  1. Same unqualified name on both sides AND the name is one of the
//     well-known Go idioms above. A junior eng's tiny CLI repo otherwise
//     gets a "main similar to Execute" and "init similar to init" pair
//     for every cobra subcommand.
//  2. Both sides are package-level variables with names ending in "Cmd"
//     (e.g. rootCmd, shoutCmd, tokenCmd). These are cobra.Command{.}
//     literals - the structural similarity is intentional repetition,
//     not a refactor target.
//
// SymbolPaths arrive as full paths (e.g. "Greeter.Hello", "cmd.shoutCmd");
// we compare the last segment so package qualification doesn't defeat
// the filter.
func isIdiomaticAutolinkNoise(srcSym, tgtSym, srcKind, tgtKind string) bool {
	srcName := lastSymbolSegment(srcSym)
	tgtName := lastSymbolSegment(tgtSym)
	if srcName != "" && srcName == tgtName {
		if _, idiomatic := idiomaticIdenticalNames[srcName]; idiomatic {
			return true
		}
	}
	if srcKind == "variable" && tgtKind == "variable" &&
		strings.HasSuffix(srcName, "Cmd") && strings.HasSuffix(tgtName, "Cmd") {
		return true
	}
	return false
}

// lastSymbolSegment returns the rightmost dot-separated segment of a
// symbol path. "Greeter.Hello" -> "Hello"; "init" -> "init".
func lastSymbolSegment(sym string) string {
	if i := strings.LastIndex(sym, "."); i >= 0 {
		return sym[i+1:]
	}
	return sym
}
