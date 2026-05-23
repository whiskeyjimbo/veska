package mcp

import (
	"context"
	"encoding/json"
	"fmt"
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
}

// RegisterWikiTools registers the wiki surface tools. svc and repoRoot are
// required; when either is nil the tool is still registered but returns
// InternalError on every call, keeping the registry uniform across
// composition roots that have not wired the git adapter.
func RegisterWikiTools(r *Registry, svc *wiki.HotZoneService, repoRoot RepoRootFunc, repos application.RepoLister) {
	r.MustRegister(ToolSpec{
		Name:        "eng_get_hot_zone",
		Description: "Return the top-N files ranked by change risk (recent change frequency multiplied by blast radius).",
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
		Description: "Return low-risk symbols a newcomer or agent can safely start from (adjacent test, small blast radius, no open findings).",
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
		if rpcErr := checkRequired("repo_id", p.RepoID, "branch", p.Branch); rpcErr != nil {
			return nil, rpcErr
		}
		repoID, rpcErr := resolveRepoID(ctx, repos, p.RepoID)
		if rpcErr != nil {
			return nil, rpcErr
		}
		p.RepoID = repoID
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
		if rpcErr := checkRequired("repo_id", p.RepoID, "branch", p.Branch); rpcErr != nil {
			return nil, rpcErr
		}
		repoID, rpcErr := resolveRepoID(ctx, repos, p.RepoID)
		if rpcErr != nil {
			return nil, rpcErr
		}
		p.RepoID = repoID
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
		return HotZoneResponse{
			RepoID: rep.RepoID,
			Branch: rep.Branch,
			Zones:  rep.Zones,
		}, nil
	}
}
