package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	application "github.com/whiskeyjimbo/veska/internal/application"
	"github.com/whiskeyjimbo/veska/internal/application/wiki"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

// HotZoneResponse is the envelope returned by eng_get_hot_zone. It carries
// the same ranked Report the docs/veska/hot_zones.md page is built from,
// so the tool and the page can never diverge (AC3).
type HotZoneResponse struct {
	RepoID string         `json:"repo_id"`
	Branch string         `json:"branch"`
	Zones  []wiki.HotZone `json:"zones"`
	// DegradedReasons surfaces in-band hints when the response is sparse for
	// non-obvious reasons (e.g. an empty zones list because no commits have
	// landed since registration). Tools shouldn't have to read the wiki
	// markdown to learn why the call returned nothing .
	DegradedReasons []string `json:"degraded_reasons"`
	// Hint is a one-line, caller-facing string explaining sparse output
	// . Populated only when zones is empty.
	Hint string `json:"hint,omitempty"`
}

// RegisterWikiTools registers the wiki surface tools. svc and repoRoot are
// required; when either is nil the tool is still registered but returns
// InternalError on every call, keeping the registry uniform across
// composition roots that have not wired the git adapter.
func RegisterWikiTools(r *Registry, svc *wiki.HotZoneService, repoRoot RepoRootFunc, repos application.RepoLister) {
	r.MustRegister(ToolSpec{
		Name:        "eng_get_hot_zone",
		Description: "Top-N files ranked by change risk = recent-change-frequency × blast-radius. Use during PR review or onboarding to spot the load-bearing files where a small edit fans out the most.",
		InputSchema: hotZoneInputSchema,
		Handler:     makeHotZoneHandler(svc, repoRoot, repos),
	})
}

// EntryPointsResponse is the envelope returned by eng_get_entry_points. It
// carries the same selected list the docs/veska/entry_points.md page is
// built from, so the tool and the page can never diverge (AC3).
type EntryPointsResponse struct {
	RepoID      string            `json:"repo_id"`
	Branch      string            `json:"branch"`
	EntryPoints []wiki.EntryPoint `json:"entry_points"`
}

// RegisterEntryPointsTool registers the eng_get_entry_points wiki tool.
// svc may be nil — the tool is still registered but returns InternalError
// on every call, keeping the registry uniform across composition roots
// that have not wired the entry_points service.
func RegisterEntryPointsTool(r *Registry, svc *wiki.EntryPointsService, repos application.RepoLister) {
	r.MustRegister(ToolSpec{
		Name:        "eng_get_entry_points",
		Description: "High-fan-in symbols ranked by inbound call count — the natural entry points a newcomer (or agent) should read first to understand the repo. Exported, tested symbols rank above unexported untested ones at the same inbound count.",
		InputSchema: entryPointsInputSchema,
		Handler:     makeEntryPointsHandler(svc, repos),
	})
}

type entryPointsParams struct {
	RepoID string `json:"repo_id"`
	Branch string `json:"branch"`
	// IncludeTests opts the caller back in to Test*/Benchmark*/Example*/
	// Fuzz* functions and *_test.go entries. Default false — on a real
	// library the test corpus drowns out the actual public-API entry
	// points (solov2-bos: cobra returned ~hundreds of TestX funcs).
	IncludeTests bool `json:"include_tests,omitempty"`
	// Limit truncates the returned slice. 0 or unset returns the service
	// default. Values larger than the service default are silently capped
	// .
	Limit int `json:"limit,omitempty"`
}

func makeEntryPointsHandler(svc *wiki.EntryPointsService, repos application.RepoLister) ToolHandler {
	return func(ctx context.Context, _ domain.Actor, raw json.RawMessage) (any, *RPCError) {
		if svc == nil {
			return nil, &RPCError{
				Code:    CodeInternalError,
				Message: "entry points is not wired (service missing)",
			}
		}
		var p entryPointsParams
		if rpcErr := bindParams(raw, &p); rpcErr != nil {
			return nil, rpcErr
		}
		// solov2-ktz0: shim-injected cwd resolves repo_id when omitted.
		repoID, rpcErr := resolveRepoIDFromParams(ctx, repos, raw, p.RepoID)
		if rpcErr != nil {
			return nil, rpcErr
		}
		p.RepoID = repoID
		if br, rpcErr := resolveBranchOrActive(ctx, repos, p.RepoID, p.Branch); rpcErr != nil {
			return nil, rpcErr
		} else {
			p.Branch = br
		}
		rep, err := svc.SelectWith(ctx, p.RepoID, p.Branch, wiki.SelectOptions{
			IncludeTests: p.IncludeTests,
		})
		if err != nil {
			return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("entry points: %v", err)}
		}
		// Defence-in-depth: even when the service excludes test
		// candidates, prior promotions may have left test entries
		// in the surface. Filter again here unless the caller opted in.
		entries := rep.EntryPoints
		if !p.IncludeTests {
			entries = filterTestEntries(entries)
		}
		if p.Limit > 0 && p.Limit < len(entries) {
			entries = entries[:p.Limit]
		}
		return EntryPointsResponse{
			RepoID:      rep.RepoID,
			Branch:      rep.Branch,
			EntryPoints: entries,
		}, nil
	}
}

// filterTestEntries drops entry points whose file path ends in _test.go
// (Go convention) or whose symbol name carries a Test/Benchmark/Example/
// Fuzz prefix. Applied at the MCP layer so the wiki page generation
// (which renders the same list) can keep its current behaviour
// independently — solov2-bos affects the tool consumers, not the docs.
func filterTestEntries(in []wiki.EntryPoint) []wiki.EntryPoint {
	out := make([]wiki.EntryPoint, 0, len(in))
	for _, e := range in {
		if strings.HasSuffix(e.FilePath, "_test.go") {
			continue
		}
		name := e.SymbolName
		if strings.HasPrefix(name, "Test") ||
			strings.HasPrefix(name, "Benchmark") ||
			strings.HasPrefix(name, "Example") ||
			strings.HasPrefix(name, "Fuzz") {
			continue
		}
		out = append(out, e)
	}
	return out
}

type hotZoneParams struct {
	RepoID string `json:"repo_id"`
	Branch string `json:"branch"`
	// Limit truncates the returned slice. 0 or unset returns the service
	// default .
	Limit int `json:"limit,omitempty"`
}

func makeHotZoneHandler(svc *wiki.HotZoneService, repoRoot RepoRootFunc, repos application.RepoLister) ToolHandler {
	return func(ctx context.Context, _ domain.Actor, raw json.RawMessage) (any, *RPCError) {
		if svc == nil || repoRoot == nil {
			return nil, &RPCError{
				Code:    CodeInternalError,
				Message: "hot zone is not wired (service or repoRoot missing)",
			}
		}
		var p hotZoneParams
		if rpcErr := bindParams(raw, &p); rpcErr != nil {
			return nil, rpcErr
		}
		// solov2-ktz0: shim-injected cwd resolves repo_id when omitted.
		repoID, rpcErr := resolveRepoIDFromParams(ctx, repos, raw, p.RepoID)
		if rpcErr != nil {
			return nil, rpcErr
		}
		p.RepoID = repoID
		if br, rpcErr := resolveBranchOrActive(ctx, repos, p.RepoID, p.Branch); rpcErr != nil {
			return nil, rpcErr
		} else {
			p.Branch = br
		}
		root, err := repoRoot(ctx, p.RepoID)
		if err != nil {
			return nil, &RPCError{Code: CodeNotFound, Message: fmt.Sprintf("repo not found: %s", p.RepoID)}
		}
		if root == "" {
			return nil, &RPCError{Code: CodeNotFound, Message: fmt.Sprintf("repo has no root path: %s", p.RepoID)}
		}
		rep, err := svc.Rank(ctx, p.RepoID, p.Branch, root)
		if err != nil {
			return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("hot zone: %v", err)}
		}
		// Canonicalise file_path to absolute on the wire so every tool in
		// the eng_* surface returns the same shape . The wiki
		// markdown still renders the relative form via the same Report
		// (the Markdown is built before this loop runs).
		src := rep.Zones
		if p.Limit > 0 && p.Limit < len(src) {
			src = src[:p.Limit]
		}
		zones := make([]wiki.HotZone, len(src))
		for i, z := range src {
			abs := z.FilePath
			if !filepath.IsAbs(abs) {
				abs = filepath.Join(root, abs)
			}
			z.FilePath = abs
			zones[i] = z
		}
		// solov2-636y/solov2-z5o0: when ranking returns no zones, explain
		// why precisely instead of guessing — Rank reports both how many
		// files were touched in the look-back window (scanned) and how
		// many of those produced a non-zero score (scored). The two
		// numbers separate the "quiet repo" case from the "only lockfile
		// churn" case.
		// Non-nil so the field serializes as [] when empty .
		degraded := []string{}
		hint := ""
		if len(zones) == 0 {
			switch {
			case rep.CandidatesScanned == 0:
				degraded = append(degraded, "no_recent_commits")
				hint = "no commits in the past 30 days — hot-zone ranking is per-commit-frequency-driven, so commit some changes and re-run"
			case rep.CandidatesScored == 0:
				degraded = append(degraded, "no_scored_zones")
				hint = fmt.Sprintf("%d file(s) changed in the last 30 days but none have graph nodes (lockfiles, READMEs, generated assets) — hot-zone scores them at 0 and drops them", rep.CandidatesScanned)
			default:
				// Should be unreachable — if scored>0 we'd have zones. Be
				// defensive so the caller still gets a hint.
				degraded = append(degraded, "no_post_registration_commits")
			}
		}
		return HotZoneResponse{
			RepoID:          rep.RepoID,
			Branch:          rep.Branch,
			Zones:           zones,
			DegradedReasons: degraded,
			Hint:            hint,
		}, nil
	}
}
