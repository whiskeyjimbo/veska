package mcp

import (
	"context"
	"encoding/json"
	"fmt"

	application "github.com/whiskeyjimbo/veska/internal/application"
	"github.com/whiskeyjimbo/veska/internal/application/dependencies"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

// RegisterDependenciesTool registers eng_list_dependencies on r. svc and
// repos are both required for the tool to do anything useful; when svc is
// nil the tool is still registered but returns InternalError on every
// call, keeping the registry uniform across composition roots that have
// not wired the dependencies service (solov2-jlws).
func RegisterDependenciesTool(r *Registry, svc *dependencies.Service, repos application.RepoLister) {
	r.MustRegister(ToolSpec{
		Name:        "eng_list_dependencies",
		Description: "List external modules the repo imports, ranked by call-site usage count. Each entry carries a small sample of top call sites so an agent can pivot from 'which deps matter?' to 'show me how this dep is used' without an extra tool call.",
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
		// Always serialize as a non-nil slice — empty result is `[]`, not
		// omitted (solov2-2bdj contract).
		if res.Dependencies == nil {
			res.Dependencies = []dependencies.Dependency{}
		}
		return res, nil
	}
}
