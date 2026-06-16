// Package contextpack assembles a token-bounded bundle of code-graph context
// (nodes, commits, findings, tasks) for a symbol or task, clipping sections
// by descending priority to fit a token budget.
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

var ErrMissingDependency = errors.New("contextpack: missing required dependency")

// DefaultTokenBudget is the default token limit for context packs to fit comfortably in LLM context windows.
const DefaultTokenBudget = 8192

// PerNodeSnippetBytes guards against single large symbols consuming the entire
// token budget before section-level clipping occurs.
const PerNodeSnippetBytes = 1500

const defaultCommitWindow = 30 * 24 * time.Hour

// maxFilesForHistory limits history file counts.
const maxFilesForHistory = 25

// FindNodesFunc resolves a symbol name to its nodes, facilitating tests with storage fakes.
type FindNodesFunc func(ctx context.Context, repoID, branch, symbolName string) ([]*domain.Node, error)

// FileHistoryFunc lists commits touching a repo path.
type FileHistoryFunc func(ctx context.Context, repoRoot, path string, window time.Duration) ([]CommitInfo, error)

type OpenFindingsFunc func(ctx context.Context, repoID, branch string) (map[string]bool, error)

type ChangedFilesFunc func(ctx context.Context, repoRoot string) ([]string, error)

type NodesInFileFunc func(ctx context.Context, repoID, branch, filePath string) ([]string, error)

type ActiveTaskFunc func(ctx context.Context, repoID string) (*TaskInfo, error)

// CommitInfo projects commit data.
type CommitInfo struct {
	Hash    string    `json:"hash"`
	Author  string    `json:"author"`
	When    time.Time `json:"when"`
	Subject string    `json:"subject"`
}

type TaskInfo struct {
	TaskID     string `json:"task_id"`
	RepoID     string `json:"repo_id"`
	Tracker    string `json:"tracker,omitempty"`
	TrackerRef string `json:"tracker_ref,omitempty"`
	Title      string `json:"title"`
	Active     bool   `json:"active"`
}

type NodeInfo struct {
	NodeID   string `json:"node_id"`
	Name     string `json:"name"`
	FilePath string `json:"file_path"`
	Kind     string `json:"kind"`
	Distance int    `json:"distance"`
	Seed     bool   `json:"seed"`
	HasOpen  bool   `json:"has_open_finding"`
	// Snippet is capped to PerNodeSnippetBytes; omitted if empty.
	Snippet string `json:"snippet,omitempty"`
}

type FindingInfo struct {
	NodeID string `json:"node_id"`
}

// Pack is the returned context envelope; sections are clipped in priority order.
type Pack struct {
	RepoID string `json:"repo_id"`
	Branch string `json:"branch"`
	Mode   string `json:"mode"`
	Query  string `json:"query"`
	// Focus exposes the seed node directly.
	Focus           *NodeInfo     `json:"focus,omitempty"`
	Nodes           []NodeInfo    `json:"nodes"`
	RecentCommits   []CommitInfo  `json:"recent_commits"`
	OpenFindings    []FindingInfo `json:"open_findings"`
	Tasks           []TaskInfo    `json:"tasks"`
	EstimatedTokens int           `json:"estimated_tokens"`
	TokenBudget     int           `json:"token_budget"`
	Truncated       bool          `json:"truncated"`
}

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

type Option func(*Assembler)

// WithTokenBudget sets the token ceiling.
func WithTokenBudget(n int) Option {
	return func(a *Assembler) {
		if n > 0 {
			a.tokenBudget = n
		}
	}
}

// AssemblerDeps gathers collaborators for construction.
type AssemblerDeps struct {
	FindNodes    FindNodesFunc
	Blast        *blastradius.Service
	FileHistory  FileHistoryFunc
	OpenFindings OpenFindingsFunc
	ChangedFiles ChangedFilesFunc
	NodesInFile  NodesInFileFunc
	ActiveTask   ActiveTaskFunc
}

// NewAssembler constructs an Assembler.
func NewAssembler(deps AssemblerDeps, opts ...Option) (*Assembler, error) {
	switch {
	case deps.FindNodes == nil:
		return nil, fmt.Errorf("contextpack.NewAssembler: FindNodes is nil: %w", ErrMissingDependency)
	case deps.Blast == nil:
		return nil, fmt.Errorf("contextpack.NewAssembler: Blast is nil: %w", ErrMissingDependency)
	case deps.FileHistory == nil:
		return nil, fmt.Errorf("contextpack.NewAssembler: FileHistory is nil: %w", ErrMissingDependency)
	case deps.OpenFindings == nil:
		return nil, fmt.Errorf("contextpack.NewAssembler: OpenFindings is nil: %w", ErrMissingDependency)
	case deps.ChangedFiles == nil:
		return nil, fmt.Errorf("contextpack.NewAssembler: ChangedFiles is nil: %w", ErrMissingDependency)
	case deps.NodesInFile == nil:
		return nil, fmt.Errorf("contextpack.NewAssembler: NodesInFile is nil: %w", ErrMissingDependency)
	case deps.ActiveTask == nil:
		return nil, fmt.Errorf("contextpack.NewAssembler: ActiveTask is nil: %w", ErrMissingDependency)
	}
	a := &Assembler{
		findNodes:    deps.FindNodes,
		blast:        deps.Blast,
		fileHistory:  deps.FileHistory,
		openFindings: deps.OpenFindings,
		changedFiles: deps.ChangedFiles,
		nodesInFile:  deps.NodesInFile,
		activeTask:   deps.ActiveTask,
		tokenBudget:  DefaultTokenBudget,
	}
	for _, opt := range opts {
		opt(a)
	}
	return a, nil
}

// ForSymbol builds a context pack starting from a symbol name.
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

// ForNode builds a context pack from a node ID.
func (a *Assembler) ForNode(ctx context.Context, repoID, branch, repoRoot, nodeID string) (Pack, error) {
	pack, err := a.assemble(ctx, repoID, branch, repoRoot, []string{nodeID})
	if err != nil {
		return Pack{}, err
	}
	pack.Mode = "node"
	pack.Query = nodeID
	a.clip(&pack)
	return pack, nil
}

// ForTask builds a context pack for a task.
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

// assemble constructs the un-clipped context pack.
func (a *Assembler) assemble(ctx context.Context, repoID, branch, repoRoot string, seedIDs []string) (Pack, error) {
	pack := Pack{RepoID: repoID, Branch: branch, TokenBudget: a.tokenBudget}

	seedSet := make(map[string]struct{}, len(seedIDs))
	for _, id := range seedIDs {
		seedSet[id] = struct{}{}
	}

	var entries []blastradius.Entry
	if len(seedIDs) > 0 {
		resp, err := a.blast.Of(ctx, repoID, branch, seedIDs, blastradius.Options{
			Direction: blastradius.DirBoth,
		})
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
		ni := NodeInfo{
			NodeID:   e.NodeID,
			Name:     e.SymbolPath,
			FilePath: e.FilePath,
			Kind:     e.Kind,
			Distance: e.Distance,
			Seed:     isSeed,
			HasOpen:  hasOpen,
			Snippet:  trimSnippet(e.Snippet, PerNodeSnippetBytes),
		}
		pack.Nodes = append(pack.Nodes, ni)
		if isSeed && pack.Focus == nil {
			seedCopy := ni
			pack.Focus = &seedCopy
		}
		if e.FilePath != "" {
			fileSet[e.FilePath] = struct{}{}
		}
		if hasOpen {
			findingSet[e.NodeID] = struct{}{}
		}
	}

	for id := range findingSet {
		pack.OpenFindings = append(pack.OpenFindings, FindingInfo{NodeID: id})
	}
	sort.Slice(pack.OpenFindings, func(i, j int) bool {
		return pack.OpenFindings[i].NodeID < pack.OpenFindings[j].NodeID
	})

	// Cap the number of history files checked to bound git shell-out execution time.
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

// clip drops low-priority sections (Tasks, Findings, Commits, Nodes) sequentially until
// the pack fits the budget.
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

// clipSlice iteratively drops trailing elements of a section until the budget
// constraint is satisfied.
func clipSlice(p *Pack, more func() bool, remove func(), budget int) {
	for p.EstimatedTokens > budget && more() {
		remove()
		p.EstimatedTokens = estimateTokens(p)
	}
}

// estimateTokens estimates token counts by dividing the JSON length by 4,
// falling back to 0 on marshal errors.
func estimateTokens(p *Pack) int {
	b, err := json.Marshal(p)
	if err != nil {
		return 0
	}
	return len(b) / 4
}

// trimSnippet truncates a snippet to max bytes at a UTF-8 boundary, appending
// a truncation marker.
func trimSnippet(s string, max int) string {
	if len(s) <= max {
		return s
	}
	// Back off to a UTF-8 boundary: the byte at [max] must not be a
	// continuation byte (10xxxxxx). Walk back until it isn't.
	cut := max
	for cut > 0 && s[cut]&0xC0 == 0x80 {
		cut--
	}
	return s[:cut] + "\n...\n"
}
