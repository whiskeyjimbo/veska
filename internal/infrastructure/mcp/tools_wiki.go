package mcp

import (
	"context"
	"encoding/json"
	"fmt"

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
func RegisterWikiTools(r *Registry, svc *wiki.HotZoneService, repoRoot RepoRootFunc) {
	r.MustRegister(ToolSpec{
		Name:        "eng_get_hot_zone",
		Description: "Return the top-N files ranked by change risk (recent change frequency multiplied by blast radius).",
		Handler:     makeHotZoneHandler(svc, repoRoot),
	})
}

type hotZoneParams struct {
	RepoID string `json:"repo_id"`
	Branch string `json:"branch"`
}

func makeHotZoneHandler(svc *wiki.HotZoneService, repoRoot RepoRootFunc) ToolHandler {
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
