// SPDX-License-Identifier: AGPL-3.0-only

package summary

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// ErrMissingDependency is returned by NewHandler when a required collaborator
// is nil. It is matched with errors.Is by callers.
var ErrMissingDependency = errors.New("summary: missing required dependency")

// maxBodyRunes caps the per-node source slice fed to the model so a huge
// function does not blow the token budget; the signature plus a leading slice
// is enough to summarize intent.
const maxBodyRunes = 4000

// maxSummaryTokens bounds the model's completion: a summary is one sentence, so
// a small ceiling keeps CPU inference fast and the output on-contract.
const maxSummaryTokens = 128

// RepoRootFunc resolves a repoID to its registered working-tree path so the
// handler can read a node's source from disk (raw content is not persisted on
// nodes). It mirrors the review handler's resolver.
type RepoRootFunc func(ctx context.Context, repoID string) (string, error)

// Handler implements ports.WorkHandler for WorkKindSummary rows. One row maps
// to one promoted file: the handler loads the file's summarizable nodes, slices
// each node's body out of the on-disk source, asks the LLMGenerator for a
// one-line summary, and persists it to nodes.short_summary.
//
// Degradation (summary-worker.md §7): when the generator is unhealthy the
// handler returns an error so the poller re-queues; it never fails the
// promotion (the lane runs post-promotion) and never blocks responses (a NULL
// short_summary falls back to the heuristic at read time).
//
// The handler is stateless beyond its injected dependencies and safe for the
// poller's per-kind goroutine.
type Handler struct {
	gen      ports.LLMGenerator
	store    Store
	repoRoot RepoRootFunc

	genName string
	audit   ports.AuditWriter
	now     func() time.Time
}

// HandlerOption customises a Handler.
type HandlerOption func(*Handler)

// WithAuditWriter records a system-actor audit line per summarized file so the
// LLM-authored provenance is traceable (summary-worker.md §2 invariant 2).
func WithAuditWriter(w ports.AuditWriter) HandlerOption {
	return func(h *Handler) { h.audit = w }
}

// WithGeneratorName sets the generator label used in the audit actor id
// ("agent:<name>"). Defaults to "llm".
func WithGeneratorName(name string) HandlerOption {
	return func(h *Handler) {
		if name != "" {
			h.genName = name
		}
	}
}

// NewHandler constructs a summary Handler. gen, store, and repoRoot are
// required; a nil one yields ErrMissingDependency.
func NewHandler(gen ports.LLMGenerator, store Store, repoRoot RepoRootFunc, opts ...HandlerOption) (*Handler, error) {
	if gen == nil || store == nil || repoRoot == nil {
		return nil, ErrMissingDependency
	}
	h := &Handler{
		gen:      gen,
		store:    store,
		repoRoot: repoRoot,
		genName:  "llm",
		now:      time.Now,
	}
	for _, o := range opts {
		o(h)
	}
	return h, nil
}

// Handle processes one ports.WorkRow of kind WorkKindSummary.
func (h *Handler) Handle(ctx context.Context, row ports.WorkRow) error {
	if row.Kind != ports.WorkKindSummary {
		return fmt.Errorf("summary.Handle: unexpected kind %q", row.Kind)
	}
	filePath := row.Payload
	if filePath == "" {
		return nil
	}

	root, err := h.repoRoot(ctx, row.RepoID)
	if err != nil {
		return fmt.Errorf("summary.Handle: resolve repo root for %q: %w", row.RepoID, err)
	}
	src, err := os.ReadFile(filepath.Join(root, filePath))
	if err != nil {
		return fmt.Errorf("summary.Handle: read promoted file %q: %w", filePath, err)
	}
	lines := strings.Split(string(src), "\n")

	nodes, err := h.store.PromotedNodes(ctx, row.RepoID, row.Branch, filePath)
	if err != nil {
		return fmt.Errorf("summary.Handle: load promoted nodes for %q: %w", filePath, err)
	}

	summarized := 0
	for _, n := range nodes {
		if !summarizable(n.Kind) {
			continue
		}
		body := sliceBody(lines, n.LineStart, n.LineEnd)
		text, gerr := h.summarizeNode(ctx, row, filePath, n, body)
		if gerr != nil {
			// Re-queue: a generator error must not lose the rest of the file's
			// progress permanently, but the poller retries the whole row.
			return gerr
		}
		if err := h.store.SetShortSummary(ctx, row.RepoID, row.Branch, n.NodeID, text); err != nil {
			return fmt.Errorf("summary.Handle: persist summary for %q: %w", n.NodeID, err)
		}
		summarized++
	}

	h.auditFile(ctx, row, filePath, summarized)
	return nil
}

// summarizeNode renders the prompt for one node, dispatches it, and returns the
// parsed summary truncated to the rune budget. An empty model summary falls
// back to the node's heuristic so the column is never written empty.
func (h *Handler) summarizeNode(ctx context.Context, row ports.WorkRow, filePath string, n Node, body string) (string, error) {
	prompt := renderPrompt(row.RepoID, row.Branch, filePath, n, body)
	resp, err := h.gen.Generate(ctx, ports.GenerateRequest{
		Prompt:                prompt,
		MaxTokens:             maxSummaryTokens,
		Format:                summarySchema,
		PromptTemplateVersion: promptVersion,
	})
	if err != nil {
		return "", fmt.Errorf("summary.Handle: generate for %q: %w", n.NodeID, err)
	}
	text := parseSummary(resp.Text)
	if text == "" {
		text = heuristic(n)
	}
	return domain.TruncateRunes(text, domain.MaxShortSummaryRunes), nil
}

// auditFile records one system-actor audit line for a summarized file. A write
// failure is logged, not propagated: the summaries are already persisted.
func (h *Handler) auditFile(ctx context.Context, row ports.WorkRow, filePath string, n int) {
	if h.audit == nil || n == 0 {
		return
	}
	if err := h.audit.Write(ctx, ports.AuditEntry{
		RepoID:    row.RepoID,
		ActorID:   "agent:" + h.genName,
		ActorKind: domain.ActorKindSystem,
		Op:        "summary.write",
		TargetID:  filePath,
		Branch:    row.Branch,
		CreatedAt: h.now(),
		Reason:    fmt.Sprintf("summarized %d node(s)", n),
	}); err != nil {
		slog.Error("summary: write audit line", "file", filePath, "err", err)
	}
}

// sliceBody returns the 1-indexed [start,end] line slice of lines, capped to
// maxBodyRunes. Out-of-range bounds clamp to the available lines.
func sliceBody(lines []string, start, end int) string {
	if start < 1 {
		start = 1
	}
	if end > len(lines) {
		end = len(lines)
	}
	if start > end {
		return ""
	}
	return domain.TruncateRunes(strings.Join(lines[start-1:end], "\n"), maxBodyRunes)
}

// heuristic mirrors domain.Node.HeuristicSummary for the summary.Node
// projection, used as the fallback when the model returns nothing usable.
func heuristic(n Node) string {
	s := strings.TrimSpace(n.Signature)
	if s == "" {
		s = strings.TrimSpace(n.Kind + " " + n.Name)
	}
	return domain.TruncateRunes(s, domain.MaxShortSummaryRunes)
}
