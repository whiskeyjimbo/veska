// Package coverage holds the frozen golden fixture's known-facts manifest:
// the single source of truth that every eng_* tool coverage subtest asserts
// against (solov2-5zka). The fixture source trees live under testdata/ as two
// self-contained Go modules; this package indexes them through the real
// cold-scan pipeline and freezes the actually-extracted facts here.
//
// # Two kinds of facts
//
//   - PARSE-DERIVED facts (Nodes, Edges, CrossRepoEdges, EntryPoints, Todos)
//     were AUTHORED FROM REAL PIPELINE OUTPUT, not hand-predicted. They are
//     guarded against fixture drift by the self-test in manifest_test.go,
//     which re-indexes the fixture and asserts every one is present (and that
//     each NodeKey resolves byte-for-byte to the node_id the pipeline emits).
//
//   - SEED-STATE facts (Repos, Aliases, Tasks, Findings, Suppressions) are
//     NOT parse output. They are the literal operational state a coverage
//     harness will INSERT before exercising the registry / task / finding /
//     suppression tools. They are declared here so the schema spans every
//     fact category the 40 eng_* tools assert, but the drift self-test does
//     not assert them (there is nothing to re-derive from a parse).
//
// # Amendment, not forking (F1)
//
// Later tool beads amend this manifest in place when a fact a tool needs is
// missing — they never fork it. 5zka delivers the amendable base + schema and
// freezes the facts for the fixture as built; it does NOT author all 40 tools'
// exact assertions.
package coverage

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

const (
	// AlphaRepoID and BetaRepoID are the repo identifiers under which the two
	// fixture modules are indexed. They are part of the node-ID key material
	// (see NodeKey.ResolveID) and so are frozen constants, not test-local.
	AlphaRepoID = "fixture-modalpha"
	BetaRepoID  = "fixture-modbeta"

	// AlphaModulePath / BetaModulePath are the Go module paths declared in each
	// fixture module's go.mod. modbeta's repo MUST be registered with
	// BetaModulePath so the promoter recognises example.com/modalpha/metric as
	// an external module and emits a cross-repo edge stub.
	AlphaModulePath = "example.com/modalpha"
	BetaModulePath  = "example.com/modbeta"

	// FixtureBranch is the branch every fixture node/edge is promoted on.
	FixtureBranch = "main"
)

// NodeKey identifies a node by its stable, machine-independent coordinates:
// the repo-RELATIVE slash path, the kind, and the symbol name. The manifest
// NEVER stores raw sha256 node IDs or absolute paths — a harness resolves keys
// to IDs at test time via ResolveID, supplying the root it indexed at.
//
// Name mirrors the parser's node name, NOT the symbol_path: methods are keyed
// "Type.method" (e.g. "Badge.RenderBadge"), packages by the package name.
type NodeKey struct {
	// Path is the repo-relative path in slash form, e.g. "metric/series.go".
	Path string
	Kind domain.NodeKind
	Name string
}

// ResolveID reconstructs the sha256 node ID the cold-scan pipeline emits for
// this key when the repo was indexed at absolute filesystem root.
//
// IMPORTANT: the pipeline keys node IDs on the ABSOLUTE path it walked
// (coldscan.go fileStager.stage passes the absolute path, not the relative
// one — the line-227 `rel` is used only for .veskaignore matching). So the
// manifest freezes the relative Path and ResolveID rejoins it onto the
// caller-supplied root with filepath.Join (the same call WalkDir used), giving
// the exact byte sequence treesitter nodeID() hashed. Do NOT ToSlash here —
// nodeID hashes filepath.Join output verbatim.
//
// This signature intentionally takes (repoID, root) — it diverges from a
// root-free ResolveID(repoID) because production paths are absolute; the
// coverage harness must pass the per-repo root it indexed at.
func (k NodeKey) ResolveID(repoID, root string) domain.NodeID {
	abs := filepath.Join(root, filepath.FromSlash(k.Path))
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "%s\x00%s\x00%s\x00%s", repoID, abs, string(k.Kind), k.Name)
	return domain.NodeID(hex.EncodeToString(h.Sum(nil)))
}

// EdgeFact is a parse-derived edge between two NodeKeys. Src and Dst are keyed
// so the harness resolves both endpoints to IDs without pasting any sha.
type EdgeFact struct {
	RepoID string
	Kind   domain.EdgeKind
	Src    NodeKey
	Dst    NodeKey
}

// CrossRepoEdgeFact is a cross-module call that the promoter records as a
// cross_repo_edge_stub row (module_path + symbol_path) rather than a resolved
// edge, because the callee lives in another registered repo/module. This is
// the genuine cross-module dependency fact the cross-repo tools assert on.
type CrossRepoEdgeFact struct {
	RepoID     string          // repo that owns the call site
	Kind       domain.EdgeKind // CALLS
	Src        NodeKey         // call-site node in RepoID
	ModulePath string          // target module path, e.g. "example.com/modalpha/metric"
	Symbol     string          // target symbol, e.g. "ComputeVariance"
}

// DependencyFact is a file→import-path dependency the promoter recorded in the
// file_imports table. It is the import-level signal eng_list_dependencies
// unions with the cross_repo_edge_stub call-level signal (CrossRepoEdgeFact);
// the two surfaces are distinct (imports vs resolved call sites), hence both
// categories. Only paths isExternalModulePath accepts are stored, so intra-
// stdlib imports (e.g. "net/http", "fmt") are absent by design. The repo's
// own-module imports (e.g. "example.com/modbeta/widget") are also absent:
// syncFileImports subtracts the repo's own module_path so intra-module imports
// never count as dependencies (solov2-tb74).
type DependencyFact struct {
	RepoID      string
	FromRelPath string // repo-relative slash path of the importing file
	ImportPath  string // the imported package path
}

// EntryPointFact is a node isEntryPointKind selects as a program/service
// entry point (eng_get_entry_points).
type EntryPointFact struct {
	RepoID string
	Node   NodeKey
}

// TodoFact is a TODO/FIXME marker the parser's lexical scan surfaced and the
// ingester collapsed into one rule='todo' finding per file. RelPath is the
// repo-relative slash path of the containing file; Line is the 1-based source
// line; Marker is the matched token; Text is the trailing comment text.
//
// The persisted finding message embeds the ABSOLUTE file path, so the
// self-test asserts on (repo, relative path suffix, line) rather than the full
// message string.
type TodoFact struct {
	RepoID  string
	RelPath string
	Line    int
	Marker  string
	Text    string
}

// CloneFact names a near-duplicate pair the clone detector should relate
// (eng_find_clones). The two members share structure with only identifier
// differences. Frozen as keys, not scores — the exact similarity is the
// clone tool's concern, not this manifest's.
type CloneFact struct {
	RepoID string
	A      NodeKey
	B      NodeKey
}

// --- SEED-STATE fact types (NOT parse output; inserted by the harness) ---

// RepoFact is a repos-table registry row a harness seeds before exercising the
// repo-registry tools (eng_add_repo, eng_list_repos, eng_get_repo, ...).
type RepoFact struct {
	RepoID     string
	RootPath   string // resolved at harness time to the testdata module root
	ModulePath string
	Branch     string
}

// AliasFact is a repo alias seed (eng_set_repo_alias / eng_get_repo via alias).
type AliasFact struct {
	Alias  string
	RepoID string
}

// TaskFact is an active-task / task-history seed (eng_set_active_task,
// eng_get_active_task, eng_get_task_history).
type TaskFact struct {
	RepoID      string
	TaskID      string
	Description string
	Active      bool
}

// FindingFact is a seeded finding row for the finding lifecycle tools
// (eng_list_findings, eng_get_finding, eng_close_finding, eng_reopen_finding).
// It is distinct from the parse-derived TodoFacts: those are emitted by the
// pipeline, these are arbitrary findings a harness inserts to drive the
// finding tools deterministically.
type FindingFact struct {
	RepoID   string
	Rule     string
	Severity string
	Message  string
	Anchor   NodeKey // the node the finding is anchored to
	State    string  // "open" | "closed"
}

// SuppressionFact is a seeded suppression for the suppression tools
// (eng_suppress_finding, eng_get_suppression, eng_list_suppressions,
// eng_close_suppression).
type SuppressionFact struct {
	RepoID string
	Rule   string
	Anchor NodeKey
	Reason string
}

// Facts is the whole known-facts manifest. Parse-derived slices are frozen
// from real pipeline output; seed-state slices are literal harness input.
type Facts struct {
	// Parse-derived (drift-guarded by manifest_test.go).
	Nodes          []NodeKey
	Edges          []EdgeFact
	CrossRepoEdges []CrossRepoEdgeFact
	Dependencies   []DependencyFact
	EntryPoints    []EntryPointFact
	Todos          []TodoFact
	Clones         []CloneFact

	// Seed-state (inserted by the harness; not parse output).
	Repos        []RepoFact
	Aliases      []AliasFact
	Tasks        []TaskFact
	Findings     []FindingFact
	Suppressions []SuppressionFact
}

// Manifest returns the frozen golden-fixture facts. It is a function (not a
// package var) so callers cannot mutate the shared facts.
func Manifest() Facts {
	return Facts{
		Nodes:          frozenNodes(),
		Edges:          frozenEdges(),
		CrossRepoEdges: frozenCrossRepoEdges(),
		Dependencies:   frozenDependencies(),
		EntryPoints:    frozenEntryPoints(),
		Todos:          frozenTodos(),
		Clones:         frozenClones(),
		Repos:          seedRepos(),
		Aliases:        seedAliases(),
		Tasks:          seedTasks(),
		Findings:       seedFindings(),
		Suppressions:   seedSuppressions(),
	}
}
