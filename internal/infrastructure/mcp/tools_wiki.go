// SPDX-License-Identifier: AGPL-3.0-only

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
	"github.com/whiskeyjimbo/veska/internal/core/protocol"
	gitinfra "github.com/whiskeyjimbo/veska/internal/infrastructure/git"
)

// HotZoneResponse is the envelope returned by eng_get_hot_zone.
type HotZoneResponse struct {
	RepoID string         `json:"repo_id"`
	Branch string         `json:"branch"`
	Zones  []wiki.HotZone `json:"zones"`
	// DegradedReasons surfaces in-band hints when the response is sparse.
	DegradedReasons []string `json:"degraded_reasons"`
	// Hint explains the cause of an empty response.
	Hint string `json:"hint,omitempty"`
}

// RegisterWikiTools registers the wiki surface tools in the registry.
func RegisterWikiTools(r *Registry, svc *wiki.HotZoneService, repoRoot RepoRootFunc, repos application.RepoLister) {
	r.MustRegister(ToolSpec{
		Name:        "eng_get_hot_zone",
		Description: "Top-N files ranked by change risk = recent-change-frequency × blast-radius. Use during PR review or onboarding to spot the load-bearing files where a small edit fans out the most.",
		InputSchema: hotZoneInputSchema,
		Handler:     makeHotZoneHandler(svc, repoRoot, repos),
	})
}

// EntryPointsResponse is the envelope returned by eng_get_entry_points.
type EntryPointsResponse struct {
	RepoID      string            `json:"repo_id"`
	Branch      string            `json:"branch"`
	EntryPoints []wiki.EntryPoint `json:"entry_points"`
}

// RegisterEntryPointsTool registers the eng_get_entry_points wiki tool in the registry.
func RegisterEntryPointsTool(r *Registry, svc *wiki.EntryPointsService, repos application.RepoLister) {
	r.MustRegister(ToolSpec{
		Name:        "eng_get_entry_points",
		Description: "High-fan-in symbols ranked by inbound call count - the natural entry points a newcomer (or agent) should read first to understand the repo. Exported, tested symbols rank above unexported untested ones at the same inbound count.",
		InputSchema: entryPointsInputSchema,
		Handler:     makeEntryPointsHandler(svc, repos),
	})
}

type entryPointsParams struct {
	RepoID string `json:"repo_id"`
	Branch string `json:"branch"`
	// IncludeTests includes test and benchmark files in the results.
	IncludeTests bool `json:"include_tests,omitempty"`
	// Limit limits the number of returned results.
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

		entries := rep.EntryPoints
		if !p.IncludeTests {
			entries = filterTestEntries(entries)
		}
		if p.Limit > 0 && p.Limit < len(entries) {
			entries = entries[:p.Limit]
		}

		if root, ok := repoRoot(ctx, repos, p.RepoID); ok {
			absEntries := make([]wiki.EntryPoint, len(entries))
			for i, e := range entries {
				if !filepath.IsAbs(e.FilePath) {
					e.FilePath = filepath.Join(root, e.FilePath)
				}
				absEntries[i] = e
			}
			entries = absEntries
		}
		return EntryPointsResponse{
			RepoID:      rep.RepoID,
			Branch:      rep.Branch,
			EntryPoints: entries,
		}, nil
	}
}

// filterTestEntries filters out test, benchmark, example, and fuzz entries from the results.
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
	// Limit limits the number of returned results.
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

		degraded := []string{}
		hint := ""
		// Hot-zone ranking is pure git-churn; on a shallow clone every file
		// scores freq=1, so the ranking is noise. Flag it whether or not zones
		// came back.
		if shallow, serr := gitinfra.IsShallow(ctx, root); serr == nil && shallow {
			degraded = AppendDegradedReason(degraded, protocol.DegradedReasonShallowClone)
		}
		if len(zones) == 0 {
			switch {
			case rep.CandidatesScanned == 0:
				degraded = append(degraded, "no_recent_commits")
				hint = "no commits in the past 30 days - hot-zone ranking is per-commit-frequency-driven, so commit some changes and re-run"
			case rep.CandidatesScored == 0:
				degraded = append(degraded, "no_scored_zones")
				hint = fmt.Sprintf("%d file(s) changed in the last 30 days but none have graph nodes (lockfiles, READMEs, generated assets) - hot-zone scores them at 0 and drops them", rep.CandidatesScanned)
			default:
				// Should be unreachable - if scored>0 we'd have zones. Be
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
