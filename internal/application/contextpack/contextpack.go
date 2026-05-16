// Package contextpack assembles a token-bounded bundle of code-graph
// context — relevant nodes, recent commits, open findings and tasks —
// for a single symbol or a single task. It backs the eng_get_context_pack
// MCP tool (M4 epic, m4.01).
//
// Two input modes are supported:
//
//   - {symbol}: relevant nodes = the FindNodes(symbol) result plus its
//     blast radius; recent commits = FileHistory for those nodes' files;
//     open findings = FindingQuerier scoped to those nodes; tasks = the
//     repo's active task.
//   - {task_id}: domain.Task carries no graph link, so the repo's
//     working-tree diff (ChangedFiles) is treated as the symbol set —
//     relevant nodes = nodes in the changed files (NodesInFile); commits,
//     findings and tasks are derived from that node set plus the task.
//
// The assembled bundle is clipped to a configurable token budget
// (DefaultTokenBudget) using a deterministic byte-length heuristic:
// lowest-priority sections are dropped/clipped first so an oversized
// bundle is truncated, never rejected.
//
// The Assembler depends only on injected function types and the
// blastradius application service — never on internal/infrastructure —
// so the domain/application layering stays intact (make layercheck).
package contextpack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/whiskeyjimbo/veska/internal/application/blastradius"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

// ErrMissingDependency is returned by NewAssembler when a required
// dependency is nil. It wraps so callers can errors.Is against it.
var ErrMissingDependency = errors.New("contextpack: missing required dependency")

// DefaultTokenBudget is the token ceiling applied when no WithTokenBudget
// option is set. It mirrors a comfortable LLM context slice.
const DefaultTokenBudget = 8192

// defaultCommitWindow is the look-back applied to FileHistory.
const defaultCommitWindow = 30 * 24 * time.Hour

// maxFilesForHistory caps how many distinct files are walked for commit
// history; without it a wide blast radius would shell out to git once
// per file and blow the latency budget.
const maxFilesForHistory = 25

// FindNodesFunc resolves an exact symbol name to its nodes. It mirrors
// ports.GraphStorage.FindNodes so the SQLite adapter plugs in while tests
// pass a deterministic fake.
type FindNodesFunc func(ctx context.Context, repoID, branch, symbolName string) ([]*domain.Node, error)

// FileHistoryFunc returns the commits that touched a repoRoot-relative
// path within window, newest first. It mirrors git.FileHistory.
type FileHistoryFunc func(ctx context.Context, repoRoot, path string, window time.Duration) ([]CommitInfo, error)

// OpenFindingsFunc returns the set of node IDs carrying an open finding.
// It mirrors ports.FindingQuerier.OpenFindingNodeIDs.
type OpenFindingsFunc func(ctx context.Context, repoID, branch string) (map[string]bool, error)

// ChangedFilesFunc returns the working-tree diff vs HEAD for repoRoot. It
// mirrors git.ChangedFiles.
type ChangedFilesFunc func(ctx context.Context, repoRoot string) ([]string, error)

// NodesInFileFunc resolves a repoRoot-relative file path to the node IDs
// it contains. It mirrors ports.NodeLookup.NodesInFile.
type NodesInFileFunc func(ctx context.Context, repoID, branch, filePath string) ([]string, error)

// ActiveTaskFunc returns the repo's active task, or (nil, nil) when none
// is active. It is satisfied by a narrow read over the tasks table.
type ActiveTaskFunc func(ctx context.Context, repoID string) (*TaskInfo, error)

// CommitInfo is one commit in the recent-commits section. It is the
// transport-shaped projection of git.Commit (the application layer must
// not import the git adapter).
type CommitInfo struct {
	Hash    string    `json:"hash"`
	Author  string    `json:"author"`
	When    time.Time `json:"when"`
	Subject string    `json:"subject"`
}

// TaskInfo is one task in the tasks section.
type TaskInfo struct {
	TaskID     string `json:"task_id"`
	RepoID     string `json:"repo_id"`
	Tracker    string `json:"tracker,omitempty"`
	TrackerRef string `json:"tracker_ref,omitempty"`
	Title      string `json:"title"`
	Active     bool   `json:"active"`
}

// NodeInfo is one relevant node in the bundle.
type NodeInfo struct {
	NodeID   string `json:"node_id"`
	Name     string `json:"name"`
	Path     string `json:"path"`
	Kind     string `json:"kind"`
	Distance int    `json:"distance"`
	Seed     bool   `json:"seed"`
	HasOpen  bool   `json:"has_open_finding"`
}

// FindingInfo is one open finding reference in the bundle.
type FindingInfo struct {
	NodeID string `json:"node_id"`
}

// Pack is the assembled context bundle. It is the structure the
// eng_get_context_pack MCP tool returns; the four sections are ordered by
// ascending truncation priority — Tasks and Findings are clipped before
// Commits, which is clipped before Nodes.
type Pack struct {
	RepoID          string        `json:"repo_id"`
	Branch          string        `json:"branch"`
	Mode            string        `json:"mode"`
	Query           string        `json:"query"`
	Nodes           []NodeInfo    `json:"nodes"`
	RecentCommits   []CommitInfo  `json:"recent_commits"`
	OpenFindings    []FindingInfo `json:"open_findings"`
	Tasks           []TaskInfo    `json:"tasks"`
	EstimatedTokens int           `json:"estimated_tokens"`
	TokenBudget     int           `json:"token_budget"`
	Truncated       bool          `json:"truncated"`
}

// Assembler builds context packs. It is stateless; the same instance is
// safe for concurrent callers.
type Assembler struct {
	findNodes    FindNodesFunc
	blast        *blastradius.Service
	fileHistory  FileHistoryFunc
	openFindings OpenFindingsFunc
	changedFiles ChangedFilesFunc
	nodesInFile  NodesInFileFunc
	activeTask   ActiveTaskFunc
	tokenBudget  int
}

// Option configures an Assembler at construction time.
type Option func(*Assembler)

// WithTokenBudget sets the token ceiling the bundle is clipped to.
// Non-positive values are ignored so DefaultTokenBudget stays in effect.
func WithTokenBudget(n int) Option {
	return func(a *Assembler) {
		if n > 0 {
			a.tokenBudget = n
		}
	}
}

// NewAssembler constructs an Assembler. All function dependencies and the
// blastradius service are required; a nil dependency yields an error
// wrapping ErrMissingDependency and a nil *Assembler.
func NewAssembler(
	findNodes FindNodesFunc,
	blast *blastradius.Service,
	fileHistory FileHistoryFunc,
	openFindings OpenFindingsFunc,
	changedFiles ChangedFilesFunc,
	nodesInFile NodesInFileFunc,
	activeTask ActiveTaskFunc,
	opts ...Option,
) (*Assembler, error) {
	switch {
	case findNodes == nil:
		return nil, fmt.Errorf("contextpack.NewAssembler: findNodes is nil: %w", ErrMissingDependency)
	case blast == nil:
		return nil, fmt.Errorf("contextpack.NewAssembler: blast is nil: %w", ErrMissingDependency)
	case fileHistory == nil:
		return nil, fmt.Errorf("contextpack.NewAssembler: fileHistory is nil: %w", ErrMissingDependency)
	case openFindings == nil:
		return nil, fmt.Errorf("contextpack.NewAssembler: openFindings is nil: %w", ErrMissingDependency)
	case changedFiles == nil:
		return nil, fmt.Errorf("contextpack.NewAssembler: changedFiles is nil: %w", ErrMissingDependency)
	case nodesInFile == nil:
		return nil, fmt.Errorf("contextpack.NewAssembler: nodesInFile is nil: %w", ErrMissingDependency)
	case activeTask == nil:
		return nil, fmt.Errorf("contextpack.NewAssembler: activeTask is nil: %w", ErrMissingDependency)
	}
	a := &Assembler{
		findNodes:    findNodes,
		blast:        blast,
		fileHistory:  fileHistory,
		openFindings: openFindings,
		changedFiles: changedFiles,
		nodesInFile:  nodesInFile,
		activeTask:   activeTask,
		tokenBudget:  DefaultTokenBudget,
	}
	for _, opt := range opts {
		opt(a)
	}
	return a, nil
}

// ForSymbol assembles a context pack seeded on an exact symbol name. The
// FindNodes result is expanded by blast radius for the relevant-nodes
// section; commits, findings and the active task are derived from there.
func (a *Assembler) ForSymbol(ctx context.Context, repoID, branch, repoRoot, symbol string) (Pack, error) {
	seeds, err := a.findNodes(ctx, repoID, branch, symbol)
	if err != nil {
		return Pack{}, fmt.Errorf("contextpack: find nodes: %w", err)
	}
	seedIDs := make([]string, 0, len(seeds))
	for _, n := range seeds {
		seedIDs = append(seedIDs, string(n.ID))
	}
	pack, err := a.assemble(ctx, repoID, branch, repoRoot, seedIDs)
	if err != nil {
		return Pack{}, err
	}
	pack.Mode = "symbol"
	pack.Query = symbol
	a.clip(&pack)
	return pack, nil
}

// ForTask assembles a context pack for a task. domain.Task has no graph
// link, so the repo's working-tree diff is used as the seed set: relevant
// nodes are the nodes in the changed files.
func (a *Assembler) ForTask(ctx context.Context, repoID, branch, repoRoot, taskID string) (Pack, error) {
	files, err := a.changedFiles(ctx, repoRoot)
	if err != nil {
		return Pack{}, fmt.Errorf("contextpack: changed files: %w", err)
	}
	seedSet := make(map[string]struct{})
	for _, f := range files {
		ids, err := a.nodesInFile(ctx, repoID, branch, f)
		if err != nil {
			return Pack{}, fmt.Errorf("contextpack: nodes in %s: %w", f, err)
		}
		for _, id := range ids {
			seedSet[id] = struct{}{}
		}
	}
	seedIDs := make([]string, 0, len(seedSet))
	for id := range seedSet {
		seedIDs = append(seedIDs, id)
	}
	sort.Strings(seedIDs)
	pack, err := a.assemble(ctx, repoID, branch, repoRoot, seedIDs)
	if err != nil {
		return Pack{}, err
	}
	pack.Mode = "task"
	pack.Query = taskID
	a.clip(&pack)
	return pack, nil
}

// assemble builds the un-clipped pack from a seed node-ID set. Mode and
// Query are filled by the caller.
func (a *Assembler) assemble(ctx context.Context, repoID, branch, repoRoot string, seedIDs []string) (Pack, error) {
	pack := Pack{RepoID: repoID, Branch: branch, TokenBudget: a.tokenBudget}

	seedSet := make(map[string]struct{}, len(seedIDs))
	for _, id := range seedIDs {
		seedSet[id] = struct{}{}
	}

	// Relevant nodes = seeds + blast radius.
	var entries []blastradius.Entry
	if len(seedIDs) > 0 {
		resp, err := a.blast.Of(ctx, repoID, branch, seedIDs, blastradius.Options{})
		if err != nil {
			return Pack{}, fmt.Errorf("contextpack: blast radius: %w", err)
		}
		entries = resp.Entries
	}

	flagged, err := a.openFindings(ctx, repoID, branch)
	if err != nil {
		return Pack{}, fmt.Errorf("contextpack: open findings: %w", err)
	}

	fileSet := make(map[string]struct{})
	findingSet := make(map[string]struct{})
	for _, e := range entries {
		_, isSeed := seedSet[e.NodeID]
		hasOpen := flagged[e.NodeID]
		pack.Nodes = append(pack.Nodes, NodeInfo{
			NodeID:   e.NodeID,
			Name:     symbolLeaf(e.SymbolPath),
			Path:     e.FilePath,
			Kind:     e.Kind,
			Distance: e.Distance,
			Seed:     isSeed,
			HasOpen:  hasOpen,
		})
		if e.FilePath != "" {
			fileSet[e.FilePath] = struct{}{}
		}
		if hasOpen {
			findingSet[e.NodeID] = struct{}{}
		}
	}
	// Nodes are already BFS-distance ordered by blastradius; keep that.

	for id := range findingSet {
		pack.OpenFindings = append(pack.OpenFindings, FindingInfo{NodeID: id})
	}
	sort.Slice(pack.OpenFindings, func(i, j int) bool {
		return pack.OpenFindings[i].NodeID < pack.OpenFindings[j].NodeID
	})

	// Recent commits: walk the distinct files of the relevant nodes,
	// capped so a wide blast radius cannot blow the latency budget.
	files := make([]string, 0, len(fileSet))
	for f := range fileSet {
		files = append(files, f)
	}
	sort.Strings(files)
	if len(files) > maxFilesForHistory {
		files = files[:maxFilesForHistory]
	}
	commitSeen := make(map[string]struct{})
	for _, f := range files {
		commits, err := a.fileHistory(ctx, repoRoot, f, defaultCommitWindow)
		if err != nil {
			return Pack{}, fmt.Errorf("contextpack: file history %s: %w", f, err)
		}
		for _, c := range commits {
			if _, dup := commitSeen[c.Hash]; dup {
				continue
			}
			commitSeen[c.Hash] = struct{}{}
			pack.RecentCommits = append(pack.RecentCommits, c)
		}
	}
	sort.SliceStable(pack.RecentCommits, func(i, j int) bool {
		if !pack.RecentCommits[i].When.Equal(pack.RecentCommits[j].When) {
			return pack.RecentCommits[i].When.After(pack.RecentCommits[j].When)
		}
		return pack.RecentCommits[i].Hash < pack.RecentCommits[j].Hash
	})

	// Tasks: the repo's active task, if any.
	task, err := a.activeTask(ctx, repoID)
	if err != nil {
		return Pack{}, fmt.Errorf("contextpack: active task: %w", err)
	}
	if task != nil {
		pack.Tasks = append(pack.Tasks, *task)
	}

	return pack, nil
}

// clip enforces the token budget. estimateTokens uses a deterministic
// byte-length heuristic (len(json)/4). When the bundle is over budget,
// the lowest-priority sections are dropped/clipped first — Tasks, then
// OpenFindings, then RecentCommits, then Nodes — until the estimate fits
// or every section is empty. Truncated records whether anything was cut.
func (a *Assembler) clip(p *Pack) {
	p.EstimatedTokens = estimateTokens(p)
	if p.EstimatedTokens <= a.tokenBudget {
		return
	}
	p.Truncated = true

	// Drop whole low-priority sections first.
	if p.Tasks != nil {
		p.Tasks = nil
		if p.EstimatedTokens = estimateTokens(p); p.EstimatedTokens <= a.tokenBudget {
			return
		}
	}
	// Clip findings, then commits, then nodes from the tail until it fits.
	clipSlice(p, func() bool { return len(p.OpenFindings) > 0 }, func() {
		p.OpenFindings = p.OpenFindings[:len(p.OpenFindings)-1]
	}, a.tokenBudget)
	if p.EstimatedTokens <= a.tokenBudget {
		return
	}
	clipSlice(p, func() bool { return len(p.RecentCommits) > 0 }, func() {
		p.RecentCommits = p.RecentCommits[:len(p.RecentCommits)-1]
	}, a.tokenBudget)
	if p.EstimatedTokens <= a.tokenBudget {
		return
	}
	clipSlice(p, func() bool { return len(p.Nodes) > 0 }, func() {
		p.Nodes = p.Nodes[:len(p.Nodes)-1]
	}, a.tokenBudget)
}

// clipSlice removes tail elements of a section (via remove) while more
// remain (more) and the pack is over budget, re-estimating each step.
func clipSlice(p *Pack, more func() bool, remove func(), budget int) {
	for p.EstimatedTokens > budget && more() {
		remove()
		p.EstimatedTokens = estimateTokens(p)
	}
}

// estimateTokens is the deterministic token heuristic: the JSON-encoded
// byte length divided by four. Encoding failure falls back to 0 so a
// pathological pack is never rejected.
func estimateTokens(p *Pack) int {
	b, err := json.Marshal(p)
	if err != nil {
		return 0
	}
	return len(b) / 4
}

// symbolLeaf returns the trailing segment of a dotted/slashed symbol
// path, or the whole string when it has no separator.
func symbolLeaf(symPath string) string {
	for i := len(symPath) - 1; i >= 0; i-- {
		if symPath[i] == '.' || symPath[i] == '/' || symPath[i] == ':' {
			return symPath[i+1:]
		}
	}
	return symPath
}
