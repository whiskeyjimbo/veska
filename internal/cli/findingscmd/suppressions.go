// SPDX-License-Identifier: AGPL-3.0-only

package findingscmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/whiskeyjimbo/veska/internal/cli/mcpclient"
)

// SuppressionView is the eng_list_suppressions / eng_get_suppression row
// projection.
type SuppressionView struct {
	SuppressionID string  `json:"suppression_id"`
	Scope         string  `json:"scope"`
	Target        string  `json:"target"`
	Branch        *string `json:"branch,omitempty"`
	Rule          *string `json:"rule,omitempty"`
	Reason        string  `json:"reason"`
	ExpiresAt     *int64  `json:"expires_at,omitempty"`
	CreatedAt     int64   `json:"created_at"`
	ActorID       string  `json:"actor_id"`
	ActorKind     string  `json:"actor_kind"`
}

// SuppressionsListParams bundles the inputs of RunSuppressionsList.
type SuppressionsListParams struct {
	RepoID      string
	Branch      string
	JSONOut     bool
	Out         io.Writer
	ErrOut      io.Writer
	ResolveRepo func(ctx context.Context, errOut io.Writer) string
}

// RunSuppressionsList wraps eng_list_suppressions for `veska findings
// suppressions list`.
func RunSuppressionsList(ctx context.Context, p SuppressionsListParams) error {
	params := map[string]any{}
	if p.RepoID != "" {
		params["repo_id"] = p.RepoID
	} else if rid := p.ResolveRepo(ctx, p.ErrOut); rid != "" {
		params["repo_id"] = rid
	}
	if p.Branch != "" {
		params["branch"] = p.Branch
	}
	var resp struct {
		Suppressions []SuppressionView `json:"suppressions"`
	}
	if err := mcpclient.Call(ctx, "eng_list_suppressions", params, &resp); err != nil {
		return fmt.Errorf("findings suppressions list: %w", err)
	}
	w := p.Out
	if p.JSONOut {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(resp)
	}
	if len(resp.Suppressions) == 0 {
		fmt.Fprintln(w, "no suppressions")
		return nil
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "SUPPRESSION_ID\tSCOPE\tTARGET\tBRANCH\tREASON")
	for _, s := range resp.Suppressions {
		br := "-"
		if s.Branch != nil {
			br = *s.Branch
		}
		reason := s.Reason
		if len(reason) > 60 {
			reason = reason[:57] + "..."
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", s.SuppressionID, s.Scope, s.Target, br, reason)
	}
	return tw.Flush()
}

// RunSuppressionsShow wraps eng_get_suppression for `veska findings
// suppressions show`.
func RunSuppressionsShow(ctx context.Context, suppressionID string, jsonOut bool, w io.Writer) error {
	var resp struct {
		Suppression SuppressionView `json:"suppression"`
	}
	if err := mcpclient.Call(ctx, "eng_get_suppression",
		map[string]any{"suppression_id": suppressionID}, &resp); err != nil {
		return fmt.Errorf("findings suppressions show: %w", err)
	}
	if jsonOut {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(resp.Suppression)
	}
	renderSuppressionHuman(w, resp.Suppression)
	return nil
}

func renderSuppressionHuman(w io.Writer, s SuppressionView) {
	fmt.Fprintf(w, "suppression_id : %s\n", s.SuppressionID)
	fmt.Fprintf(w, "scope          : %s\n", s.Scope)
	fmt.Fprintf(w, "target         : %s\n", s.Target)
	if s.Branch != nil {
		fmt.Fprintf(w, "branch         : %s\n", *s.Branch)
	}
	if s.Rule != nil {
		fmt.Fprintf(w, "rule           : %s\n", *s.Rule)
	}
	fmt.Fprintf(w, "actor          : %s (%s)\n", s.ActorID, s.ActorKind)
	fmt.Fprintf(w, "created_at     : %s\n", time.Unix(s.CreatedAt, 0).UTC().Format(time.RFC3339))
	if s.ExpiresAt != nil {
		fmt.Fprintf(w, "expires_at     : %s\n", time.Unix(*s.ExpiresAt, 0).UTC().Format(time.RFC3339))
	}
	fmt.Fprintf(w, "reason         :\n  %s\n", s.Reason)
}
