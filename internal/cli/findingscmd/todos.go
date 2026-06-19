// SPDX-License-Identifier: AGPL-3.0-only

package findingscmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/whiskeyjimbo/veska/internal/cli/mcpclient"
)

// TodosParams bundles the inputs of RunTodos. eng_find_todos resolves the repo
// from repo_id or the connecting client's cwd, so RepoID is optional.
type TodosParams struct {
	RepoID        string
	IncludeClosed bool
	JSONOut       bool
	Out           io.Writer
}

// todosResp mirrors the eng_find_todos envelope.
type todosResp struct {
	Todos []struct {
		FilePath string `json:"file_path"`
		Message  string `json:"message"`
		State    string `json:"state"`
	} `json:"todos"`
	DegradedReasons []string `json:"degraded_reasons"`
}

// RunTodos wraps eng_find_todos: the TODO/FIXME findings the promotion checks
// harvested from the indexed source.
func RunTodos(ctx context.Context, p TodosParams) error {
	params := map[string]any{}
	if p.RepoID != "" {
		params["repo_id"] = p.RepoID
	}
	if p.IncludeClosed {
		params["include_closed"] = true
	}
	var raw json.RawMessage
	if err := mcpclient.Call(ctx, "eng_find_todos", params, &raw); err != nil {
		return fmt.Errorf("todos: %w", err)
	}
	if p.JSONOut {
		enc := json.NewEncoder(p.Out)
		enc.SetIndent("", "  ")
		var pretty any
		_ = json.Unmarshal(raw, &pretty)
		return enc.Encode(pretty)
	}
	var resp todosResp
	if err := json.Unmarshal(raw, &resp); err != nil {
		return err
	}
	if len(resp.Todos) == 0 {
		fmt.Fprintln(p.Out, "no TODOs found")
		for _, d := range resp.DegradedReasons {
			fmt.Fprintf(p.Out, "[degraded: %s]\n", d)
		}
		return nil
	}
	tw := tabwriter.NewWriter(p.Out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "STATE\tLOCATION\tMESSAGE")
	for _, t := range resp.Todos {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", t.State, t.FilePath, t.Message)
	}
	_ = tw.Flush()
	for _, d := range resp.DegradedReasons {
		fmt.Fprintf(p.Out, "[degraded: %s]\n", d)
	}
	return nil
}
