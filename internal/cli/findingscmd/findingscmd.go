// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

// Package findingscmd holds the delivery-layer logic behind the `veska
// findings …` command tree: the eng_list_findings / eng_get_finding /
// eng_reopen_finding / suppression MCP calls, the severity sort + breakdown
// header, the low-severity curation, and the textual/JSON rendering of
// findings and suppressions. cmd/veska/findings.go and findings_suppress.go
// are reduced to Cobra command construction whose RunE bodies are thin calls
// into the Run helpers here (, following the cmd = glue /
// logic-in-packages pattern from /.5/.6).
// The cwd→repo resolver (autoResolveRepo) is a shared cmd-level helper
// deps.go/symbol.go/graph.go also call it - so it stays in cmd/veska and is
// injected here through the ResolveRepo seam rather than re-extracted.
package findingscmd

import (
	"context"
	"fmt"
	"io"
	"maps"

	"github.com/whiskeyjimbo/veska/internal/cli/mcpclient"
)

// FindingView is the eng_list_findings / eng_get_finding row projection.
type FindingView struct {
	FindingID   string  `json:"finding_id"`
	Branch      string  `json:"branch"`
	RepoID      string  `json:"repo_id"`
	FilePath    *string `json:"file_path,omitempty"`
	Severity    string  `json:"severity"`
	SourceLayer string  `json:"source_layer"`
	Rule        string  `json:"rule"`
	Message     string  `json:"message"`
	State       string  `json:"state"`
	CreatedAt   int64   `json:"created_at"`
	// SuppressedBy carries the suppression_id when an active suppression hides
	// this finding; populated only when include_suppressed=true was requested.
	SuppressedBy *string `json:"suppressed_by,omitempty"`
}

// ListParams bundles the inputs of RunList. ResolveRepo is the cwd→repo seam
// the cmd layer injects (passing a nil errOut suppresses the breadcrumb).
type ListParams struct {
	RepoID     string
	AllRepos   bool
	State      string
	Severity   string
	Rule       string
	Limit      int
	JSONOut    bool
	IncludeLow bool
	// IncludeSuppressed asks the daemon to surface findings hidden by an active
	// suppression (their state stays "open", so no --state value reveals them).
	IncludeSuppressed bool
	Out               io.Writer
	ErrOut            io.Writer
	ResolveRepo       func(ctx context.Context, errOut io.Writer) string
}

// RunList implements `veska findings list`: it resolves scope, gathers the
// findings (optionally fanning out across every registered repo), and renders
// them. Behaviour mirrors the prior cmd/veska findingsListCmd RunE.
func RunList(ctx context.Context, p ListParams) error {
	baseParams, fanout := p.listScope(ctx)
	resp, err := p.gatherFindings(ctx, baseParams, fanout)
	if err != nil {
		return err
	}
	return p.render(resp)
}

// listScope decides the repo + state scope. It returns the base MCP params
// (state/severity/rule filters) and whether to fan out across all repos.
func (p ListParams) listScope(ctx context.Context) (map[string]any, bool) {
	// all + --repo is no longer rejected. --repo X scopes to a
	// single repo; --all asks "include every state, not just open". When --all
	// is set without --repo, fan out across every registered repo.
	fanoutAllRepos := p.AllRepos && p.RepoID == ""
	// when neither --repo nor --all is set AND the cwd doesn't
	// resolve to a single registered repo, fall back to 'list across every
	// repo' rather than erroring with 'repo_id required'.
	autoAll := false
	if !p.AllRepos && p.RepoID == "" {
		if rid := p.ResolveRepo(ctx, nil); rid == "" {
			fanoutAllRepos = true
			autoAll = true
		}
	}
	// the advisory rides stderr so it never breaks stdout pipes;
	// under --json we drop it entirely.
	if autoAll && !p.JSONOut {
		fmt.Fprintln(p.ErrOut, "veska: no --repo and cwd outside any registered repo; listing findings across all repos (pass --repo <id> to scope)")
	}
	// all clears the default state=open filter so
	// closed/suppressed findings come back too. An explicit --state still wins.
	baseParams := map[string]any{}
	if p.State != "" {
		baseParams["state"] = p.State
	} else if p.AllRepos {
		baseParams["state"] = "any"
	}
	if p.Severity != "" {
		baseParams["severity"] = p.Severity
	}
	if p.Rule != "" {
		baseParams["rule"] = p.Rule
	}
	// a suppressed finding keeps state="open" (suppression hides
	// it rather than closing it), so no --state value surfaces it. Only
	// include_suppressed=true does - the daemon LEFT JOINs active suppressions
	// and populates suppressed_by on the returned rows.
	if p.IncludeSuppressed {
		baseParams["include_suppressed"] = true
	}
	return baseParams, fanoutAllRepos
}

// findingsEnvelope is the {findings:[.]} response shape.
type findingsEnvelope struct {
	Findings []FindingView `json:"findings"`
}

// gatherFindings issues the eng_list_findings call(s) - one per registered
// repo when fanning out, otherwise a single scoped call.
func (p ListParams) gatherFindings(ctx context.Context, baseParams map[string]any, fanout bool) (findingsEnvelope, error) {
	var resp findingsEnvelope
	if fanout {
		var lr struct {
			Repos []struct {
				RepoID  string `json:"repo_id"`
				ShortID string `json:"short_id"`
			} `json:"repos"`
		}
		if err := mcpclient.Call(ctx, "eng_list_repos", map[string]any{}, &lr); err != nil {
			return resp, fmt.Errorf("findings list --all: list repos: %w", err)
		}
		for _, r := range lr.Repos {
			params := map[string]any{"repo_id": r.RepoID}
			maps.Copy(params, baseParams)
			var part findingsEnvelope
			if err := mcpclient.Call(ctx, "eng_list_findings", params, &part); err != nil {
				fmt.Fprintf(p.ErrOut, "  warn: %s: %v\n", r.ShortID, err)
				continue
			}
			resp.Findings = append(resp.Findings, part.Findings...)
		}
		return resp, nil
	}
	params := map[string]any{}
	maps.Copy(params, baseParams)
	if p.RepoID != "" {
		params["repo_id"] = p.RepoID
	} else if rid := p.ResolveRepo(ctx, p.ErrOut); rid != "" {
		// surface which repo was picked when multiple are
		// registered so users don't get silently-scoped empty results.
		params["repo_id"] = rid
	}
	if err := mcpclient.Call(ctx, "eng_list_findings", params, &resp); err != nil {
		return resp, fmt.Errorf("findings list: %w", err)
	}
	return resp, nil
}
