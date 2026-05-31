package findingscmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/whiskeyjimbo/veska/internal/cli/mcpclient"
)

// ShowParams bundles the inputs of RunShow.
type ShowParams struct {
	FindingID string
	RepoID    string
	Branch    string
	JSONOut   bool
	Out       io.Writer
}

// RunShow wraps eng_get_finding for `veska findings show`. finding_id is
// globally unique; --repo/--branch are opt-in scoping only .
func RunShow(ctx context.Context, p ShowParams) error {
	params := map[string]any{"finding_id": p.FindingID}
	if p.RepoID != "" {
		params["repo_id"] = p.RepoID
	}
	if p.Branch != "" {
		params["branch"] = p.Branch
	}
	var resp json.RawMessage
	if err := mcpclient.Call(ctx, "eng_get_finding", params, &resp); err != nil {
		return fmt.Errorf("findings show: %w", err)
	}
	if p.JSONOut {
		var pretty any
		_ = json.Unmarshal(resp, &pretty)
		enc := json.NewEncoder(p.Out)
		enc.SetIndent("", "  ")
		return enc.Encode(pretty)
	}
	var env struct {
		Finding FindingView `json:"finding"`
	}
	if err := json.Unmarshal(resp, &env); err != nil {
		return err
	}
	RenderFindingHuman(p.Out, env.Finding)
	return nil
}

// ReopenParams bundles the inputs of RunReopen.
type ReopenParams struct {
	FindingID string
	RepoID    string
	Branch    string
	Out       io.Writer
}

// RunReopen wraps eng_reopen_finding for `veska findings reopen`. That tool
// requires both repo_id and branch (its UPDATE is repo-scoped); when the user
// didn't pass them we fetch the finding first and autofill from the row.
func RunReopen(ctx context.Context, p ReopenParams) error {
	params := map[string]any{"finding_id": p.FindingID}
	if p.RepoID != "" {
		params["repo_id"] = p.RepoID
	}
	if p.Branch != "" {
		params["branch"] = p.Branch
	}
	if p.RepoID == "" || p.Branch == "" {
		if err := autofillReopenScope(ctx, p, params); err != nil {
			return err
		}
	}
	var resp json.RawMessage
	if err := mcpclient.Call(ctx, "eng_reopen_finding", params, &resp); err != nil {
		return fmt.Errorf("findings reopen: %w", err)
	}
	fmt.Fprintln(p.Out, "reopened")
	return nil
}

// autofillReopenScope looks the finding up (defaulting the lookup branch to
// "main" when --branch is absent) and fills repo_id/branch into params from
// the resolved row. Returns a flag-pointing error when the lookup fails.
func autofillReopenScope(ctx context.Context, p ReopenParams, params map[string]any) error {
	lookupBranch := p.Branch
	if lookupBranch == "" {
		lookupBranch = "main"
	}
	var resp json.RawMessage
	if err := mcpclient.Call(ctx, "eng_get_finding",
		map[string]any{"finding_id": p.FindingID, "branch": lookupBranch}, &resp); err != nil {
		return fmt.Errorf("findings reopen: couldn't auto-resolve repo/branch (%v); pass --repo and --branch explicitly", err)
	}
	var env struct {
		Finding FindingView `json:"finding"`
	}
	_ = json.Unmarshal(resp, &env)
	if p.RepoID == "" && env.Finding.RepoID != "" {
		params["repo_id"] = env.Finding.RepoID
	}
	if p.Branch == "" && env.Finding.Branch != "" {
		params["branch"] = env.Finding.Branch
	}
	return nil
}
