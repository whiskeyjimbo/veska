// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package findingscmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/whiskeyjimbo/veska/internal/cli/mcpclient"
)

// RunClose wraps eng_close_finding for `veska findings close`. finding_id is
// globally unique; the reason is required and recorded for audit.
func RunClose(ctx context.Context, findingID, reason string, w io.Writer) error {
	if reason == "" {
		return fmt.Errorf("--reason is required")
	}
	params := map[string]any{"finding_id": findingID, "reason": reason}
	var resp json.RawMessage
	if err := mcpclient.Call(ctx, "eng_close_finding", params, &resp); err != nil {
		return fmt.Errorf("findings close: %w", err)
	}
	fmt.Fprintln(w, "closed")
	return nil
}

// SuppressParams bundles the inputs of RunSuppress.
type SuppressParams struct {
	FindingID string
	Reason    string
	Scope     string
	ExpiresAt int64
	Out       io.Writer
}

// RunSuppress wraps eng_suppress_finding for `veska findings suppress`.
// branch/repo_id are derived from the finding row by the daemon.
func RunSuppress(ctx context.Context, p SuppressParams) error {
	if p.Reason == "" {
		return fmt.Errorf("--reason is required")
	}
	params := map[string]any{"finding_id": p.FindingID, "reason": p.Reason}
	if p.Scope != "" {
		params["scope"] = p.Scope
	}
	if p.ExpiresAt != 0 {
		params["expires_at"] = p.ExpiresAt
	}
	var resp struct {
		SuppressionID string `json:"suppression_id"`
		Scope         string `json:"scope"`
		Branch        string `json:"branch"`
	}
	if err := mcpclient.Call(ctx, "eng_suppress_finding", params, &resp); err != nil {
		return fmt.Errorf("findings suppress: %w", err)
	}
	fmt.Fprintf(p.Out, "suppressed: %s (scope=%s)\n", resp.SuppressionID, resp.Scope)
	return nil
}

// RunSuppressionsClose wraps eng_close_suppression for
// `veska findings suppressions close`, expiring an active suppression now.
func RunSuppressionsClose(ctx context.Context, suppressionID, repoID string, w io.Writer) error {
	params := map[string]any{"suppression_id": suppressionID}
	if repoID != "" {
		params["repo_id"] = repoID
	}
	var resp struct {
		SuppressionID string `json:"suppression_id"`
		ExpiresAt     int64  `json:"expires_at"`
	}
	if err := mcpclient.Call(ctx, "eng_close_suppression", params, &resp); err != nil {
		return fmt.Errorf("findings suppressions close: %w", err)
	}
	fmt.Fprintf(w, "closed: %s (expires_at=%d)\n", resp.SuppressionID, resp.ExpiresAt)
	return nil
}
