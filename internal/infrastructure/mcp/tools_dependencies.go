// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package mcp

import (
	"context"
	"encoding/json"
	"fmt"

	application "github.com/whiskeyjimbo/veska/internal/application"
	"github.com/whiskeyjimbo/veska/internal/application/dependencies"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

// RegisterDependenciesTool registers eng_list_dependencies, keeping registration uniform even if the dependencies service is not wired.
func RegisterDependenciesTool(r *Registry, svc *dependencies.Service, repos application.RepoLister) {
	r.MustRegister(ToolSpec{
		Name:        "eng_list_dependencies",
		Description: "List external modules the repo CALLS into, ranked by call-site count, with sampled top call sites per module. Source is the cross_repo_edge_stubs table populated at promotion time, so " + DescDepsImportOnlyCaveat + "; this tracks the import-side backfill. Versions come from go.mod's require block. Each top_call_sites entry carries a node_id you can pass to eng_get_context_pack to see how the dependency is integrated.",
		InputSchema: listDependenciesInputSchema,
		Handler:     makeListDependenciesHandler(svc, repos),
	})
}

type listDependenciesParams struct {
	RepoID string `json:"repo_id"`
	Branch string `json:"branch"`
}

func makeListDependenciesHandler(svc *dependencies.Service, repos application.RepoLister) ToolHandler {
	return func(ctx context.Context, _ domain.Actor, raw json.RawMessage) (any, *RPCError) {
		if svc == nil {
			return nil, &RPCError{
				Code:    CodeInternalError,
				Message: "eng_list_dependencies is not wired (service missing)",
			}
		}
		var p listDependenciesParams
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
		res, err := svc.List(ctx, p.RepoID, p.Branch)
		if err != nil {
			return nil, &RPCError{Code: CodeInternalError, Message: fmt.Sprintf("list dependencies: %v", err)}
		}
		// Ensure serialization of dependencies is a non-nil slice to satisfy client expectations.
		if res.Dependencies == nil {
			res.Dependencies = []dependencies.Dependency{}
		}
		return res, nil
	}
}
