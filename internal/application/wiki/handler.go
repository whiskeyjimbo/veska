// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package wiki

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// RenderTimeStore persists and reads the wall-clock time of the most recent
// successful wiki regeneration. The application layer depends only on this
// interface; the SQLite adapter (daemon_state key 'wiki.last_render_at')
// implements it. Implementations must be safe for concurrent use.
type RenderTimeStore interface {
	// SetLastRenderAt persists the time of a successful render.
	SetLastRenderAt(ctx context.Context, t time.Time) error
	// LastRenderAt returns the most recent persisted render time. The bool
	// is false when no render has been recorded yet.
	LastRenderAt(ctx context.Context) (time.Time, bool, error)
}

// RepoRootFunc resolves a repoID to its registered working-tree path. It
// mirrors the daemon's repo-root resolver so the handler can turn the
// repoRoot-relative page-path constants into absolute paths.
type RepoRootFunc func(ctx context.Context, repoID string) (string, error)

// Handler implements ports.WorkHandler for WorkKindWiki rows. On each row it
// regenerates BOTH the hot_zone and entry_points Markdown pages and, only on
// full success, stamps the last-render time.
// Failure semantics mirror the other application handlers: any error (repo
// resolution, ranking, rendering, file write) propagates wrapped so the
// queue.Poller retry path runs. The render time is stamped ONLY when both
// pages were written - a partial failure leaves the previous stamp intact.
// The handler is stateless beyond its injected dependencies and is safe for
// concurrent use; the poller runs it in its own goroutine.
type Handler struct {
	hotZone    *HotZoneService
	entry      *EntryPointsService
	store      RenderTimeStore
	repoRoot   RepoRootFunc
	clock      func() time.Time
	writePages bool
}

// HandlerOption configures a Handler at construction time.
type HandlerOption func(*Handler)

// WithHandlerClock replaces the wall-clock used for the last-render stamp.
// Primarily for tests. The default is time.Now.
func WithHandlerClock(c func() time.Time) HandlerOption {
	return func(h *Handler) {
		if c != nil {
			h.clock = c
		}
	}
}

// WithWritePages enables Markdown page writes under in the
// user's repo work-tree. Off by default - the README contract is that
// veska does not write to user repos. The MCP tools eng_get_hot_zone and
// eng_get_entry_points still serve the same ranked data when this is off
func WithWritePages(enabled bool) HandlerOption {
	return func(h *Handler) {
		h.writePages = enabled
	}
}

// NewHandler constructs a wiki Handler. hotZone, entry, store and repoRoot
// are all required; a nil dependency yields an error wrapping
// ErrMissingDependency and a nil *Handler.
func NewHandler(hotZone *HotZoneService, entry *EntryPointsService, store RenderTimeStore, repoRoot RepoRootFunc, opts ...HandlerOption) (*Handler, error) {
	if hotZone == nil {
		return nil, fmt.Errorf("wiki.NewHandler: hotZone is nil: %w", ErrMissingDependency)
	}
	if entry == nil {
		return nil, fmt.Errorf("wiki.NewHandler: entry is nil: %w", ErrMissingDependency)
	}
	if store == nil {
		return nil, fmt.Errorf("wiki.NewHandler: store is nil: %w", ErrMissingDependency)
	}
	if repoRoot == nil {
		return nil, fmt.Errorf("wiki.NewHandler: repoRoot is nil: %w", ErrMissingDependency)
	}
	h := &Handler{
		hotZone:  hotZone,
		entry:    entry,
		store:    store,
		repoRoot: repoRoot,
		clock:    time.Now,
	}
	for _, o := range opts {
		o(h)
	}
	return h, nil
}

// Handle processes one ports.WorkRow of kind WorkKindWiki: it regenerates
// both wiki pages under and, on full success, stamps the
// last-render time.
// Behavior:
//
//	Wrong kind: wrapped error (routing bug).
//	Repo-root resolution / ranking / rendering / write error: wrapped
//	  error so the Poller retries; the render time is NOT stamped.
func (h *Handler) Handle(ctx context.Context, row ports.WorkRow) error {
	if row.Kind != ports.WorkKindWiki {
		return fmt.Errorf("wiki.Handle: unexpected kind %q", row.Kind)
	}

	root, err := h.repoRoot(ctx, row.RepoID)
	if err != nil {
		return fmt.Errorf("wiki.Handle: resolve repo root for %q: %w", row.RepoID, err)
	}

	report, err := h.hotZone.Rank(ctx, row.RepoID, row.Branch, root)
	if err != nil {
		return fmt.Errorf("wiki.Handle: rank hot zones: %w", err)
	}
	epReport, err := h.entry.Select(ctx, row.RepoID, row.Branch)
	if err != nil {
		return fmt.Errorf("wiki.Handle: select entry points: %w", err)
	}

	// stamp GeneratedAt so a Markdown page on disk never
	// hides how old it is. The render-time stamp lands in the document
	// header; the persisted SetLastRenderAt below is the source of
	// truth for staleness checks elsewhere.
	now := h.clock()
	report.GeneratedAt = now
	epReport.GeneratedAt = now

	if h.writePages {
		// committed Markdown carries repo-relative file_path
		// so the docs stay portable across machines and contributors.
		// The MCP tool responses still canonicalise to absolute (see
		// tools_wiki.go).
		if err := writePage(filepath.Join(root, HotZonesPagePath), RenderHotZones(relativizeHotZoneReport(report, root))); err != nil {
			return fmt.Errorf("wiki.Handle: write hot zones page: %w", err)
		}
		if err := writePage(filepath.Join(root, EntryPointsPagePath), RenderEntryPoints(relativizeEntryPointsReport(epReport, root))); err != nil {
			return fmt.Errorf("wiki.Handle: write entry points page: %w", err)
		}
	}
	// When writePages is false the report is still ranked and the
	// last-render stamp is bumped - the MCP tools eng_get_hot_zone /
	// eng_get_entry_points serve the same data on demand. We keep the
	// rank pass to populate any caches and to surface ranking errors at
	// the same point in the queue lifecycle.
	_ = report
	_ = epReport

	// Both pages written - stamp the last-render time. A stamp failure is
	// still a handler failure (the render is recorded as incomplete) so the
	// poller retries; the re-render is idempotent.
	if err := h.store.SetLastRenderAt(ctx, h.clock()); err != nil {
		return fmt.Errorf("wiki.Handle: persist last render time: %w", err)
	}
	return nil
}

// relativizeHotZoneReport returns a copy of report with each Zone.FilePath
// rewritten to a repoRoot-relative slash-form path when possible. Used when
// rendering the committed Markdown page so the output is portable across
// machines.
func relativizeHotZoneReport(r Report, root string) Report {
	out := r
	out.Zones = make([]HotZone, len(r.Zones))
	for i, z := range r.Zones {
		z.FilePath = relPath(root, z.FilePath)
		out.Zones[i] = z
	}
	return out
}

// relativizeEntryPointsReport mirrors relativizeHotZoneReport for the
// EntryPointsReport.
func relativizeEntryPointsReport(r EntryPointsReport, root string) EntryPointsReport {
	out := r
	out.EntryPoints = make([]EntryPoint, len(r.EntryPoints))
	for i, e := range r.EntryPoints {
		e.FilePath = relPath(root, e.FilePath)
		out.EntryPoints[i] = e
	}
	return out
}

func relPath(root, p string) string {
	if root == "" || !filepath.IsAbs(p) {
		return p
	}
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return p
	}
	return filepath.ToSlash(rel)
}

// writePage writes content to an absolute path, creating the parent
// directory if needed.
func writePage(absPath, content string) error {
	if err := os.MkdirAll(filepath.Dir(absPath), 0o755); err != nil {
		return fmt.Errorf("create dir: %w", err)
	}
	if err := os.WriteFile(absPath, []byte(content), 0o644); err != nil {
		return fmt.Errorf("write file: %w", err)
	}
	return nil
}

// Compile-time check: *Handler satisfies ports.WorkHandler.
var _ ports.WorkHandler = (*Handler)(nil)
