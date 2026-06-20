// SPDX-License-Identifier: AGPL-3.0-only

package repocmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/whiskeyjimbo/veska/internal/cli/mcpclient"
)

// showcurrent.go backs `veska repo show <id>` (eng_get_repo) and `veska repo
// current` (eng_get_current_repo). Both return a {repo: RepoView} envelope, so
// they decode into the same RepoView the list path uses and render through the
// shared PrintRepoTable for output consistency.

// repoEnvelope decodes the {repo,.} shape both tools return.
type repoEnvelope struct {
	Repo RepoView `json:"repo"`
}

// RunRepoShow wraps eng_get_repo: look up a single registered repo by id.
func RunRepoShow(ctx context.Context, w io.Writer, repoID string, jsonOut bool) error {
	var raw json.RawMessage
	if err := mcpclient.Call(ctx, "eng_get_repo", map[string]any{"repo_id": repoID}, &raw); err != nil {
		return fmt.Errorf("repo show: %w", err)
	}
	return renderRepoEnvelope(w, raw, jsonOut)
}

// RunRepoCurrent wraps eng_get_current_repo: report the repo the caller's cwd
// belongs to (or the sole registered repo when there's only one).
func RunRepoCurrent(ctx context.Context, w io.Writer, jsonOut bool) error {
	params := map[string]any{}
	if cwd, err := os.Getwd(); err == nil {
		params["cwd"] = cwd
	}
	var raw json.RawMessage
	if err := mcpclient.Call(ctx, "eng_get_current_repo", params, &raw); err != nil {
		return fmt.Errorf("repo current: %w", err)
	}
	return renderRepoEnvelope(w, raw, jsonOut)
}

func renderRepoEnvelope(w io.Writer, raw json.RawMessage, jsonOut bool) error {
	if jsonOut {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		var pretty any
		_ = json.Unmarshal(raw, &pretty)
		return enc.Encode(pretty)
	}
	var env repoEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return err
	}
	PrintRepoTable(w, []RepoView{env.Repo})
	return nil
}
