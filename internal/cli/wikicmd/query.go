package wikicmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/whiskeyjimbo/veska/internal/cli/mcpclient"
)

// query.go backs the raw-query verbs `veska entry-points` and `veska
// hot-zones`. Unlike `veska wiki` (which opens SQLite directly to write the
// markdown pages), these hit the daemon over the MCP socket and render a
// table, so an agent or script can pull the data without reading the
// generated docs. solov2-yh5a. eng_get_entry_points / eng_get_hot_zone both
// require repo_id, resolved by the Cobra layer before these run.

// EntryPointsParams bundles the inputs of RunEntryPoints.
type EntryPointsParams struct {
	RepoID       string
	IncludeTests bool
	Limit        int
	JSONOut      bool
	Out          io.Writer
}

type entryPointsResp struct {
	EntryPoints []struct {
		Name         string `json:"name"`
		FilePath     string `json:"file_path"`
		Kind         string `json:"kind"`
		InboundCount int    `json:"inbound_count"`
		Exported     bool   `json:"exported"`
	} `json:"entry_points"`
	DegradedReasons []string `json:"degraded_reasons"`
}

// RunEntryPoints wraps eng_get_entry_points: high-fan-in symbols ranked by
// inbound call count — the natural starting points for reading a repo.
func RunEntryPoints(ctx context.Context, p EntryPointsParams) error {
	params := map[string]any{}
	if p.RepoID != "" {
		params["repo_id"] = p.RepoID
	}
	if p.IncludeTests {
		params["include_tests"] = true
	}
	if p.Limit > 0 {
		params["limit"] = p.Limit
	}
	var raw json.RawMessage
	if err := mcpclient.Call(ctx, "eng_get_entry_points", params, &raw); err != nil {
		return fmt.Errorf("entry-points: %w", err)
	}
	if p.JSONOut {
		return prettyJSON(p.Out, raw)
	}
	var resp entryPointsResp
	if err := json.Unmarshal(raw, &resp); err != nil {
		return err
	}
	if len(resp.EntryPoints) == 0 {
		fmt.Fprintln(p.Out, "no entry points found")
		printDegraded(p.Out, resp.DegradedReasons)
		return nil
	}
	tw := tabwriter.NewWriter(p.Out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "INBOUND\tKIND\tNAME\tFILE")
	for _, e := range resp.EntryPoints {
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\n", e.InboundCount, e.Kind, e.Name, e.FilePath)
	}
	_ = tw.Flush()
	printDegraded(p.Out, resp.DegradedReasons)
	return nil
}

// HotZonesParams bundles the inputs of RunHotZones.
type HotZonesParams struct {
	RepoID  string
	Limit   int
	JSONOut bool
	Out     io.Writer
}

type hotZonesResp struct {
	Zones []struct {
		FilePath              string `json:"file_path"`
		RecentChangeFrequency int    `json:"recent_change_frequency"`
		BlastRadius           int    `json:"blast_radius"`
		Score                 int    `json:"score"`
	} `json:"zones"`
	DegradedReasons []string `json:"degraded_reasons"`
}

// RunHotZones wraps eng_get_hot_zone: files ranked by change risk =
// recent-change-frequency × blast-radius.
func RunHotZones(ctx context.Context, p HotZonesParams) error {
	params := map[string]any{}
	if p.RepoID != "" {
		params["repo_id"] = p.RepoID
	}
	if p.Limit > 0 {
		params["limit"] = p.Limit
	}
	var raw json.RawMessage
	if err := mcpclient.Call(ctx, "eng_get_hot_zone", params, &raw); err != nil {
		return fmt.Errorf("hot-zones: %w", err)
	}
	if p.JSONOut {
		return prettyJSON(p.Out, raw)
	}
	var resp hotZonesResp
	if err := json.Unmarshal(raw, &resp); err != nil {
		return err
	}
	if len(resp.Zones) == 0 {
		fmt.Fprintln(p.Out, "no hot zones found")
		printDegraded(p.Out, resp.DegradedReasons)
		return nil
	}
	tw := tabwriter.NewWriter(p.Out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "SCORE\tCHANGES\tBLAST\tFILE")
	for _, z := range resp.Zones {
		fmt.Fprintf(tw, "%d\t%d\t%d\t%s\n", z.Score, z.RecentChangeFrequency, z.BlastRadius, z.FilePath)
	}
	_ = tw.Flush()
	printDegraded(p.Out, resp.DegradedReasons)
	return nil
}

func prettyJSON(w io.Writer, raw json.RawMessage) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	var pretty any
	_ = json.Unmarshal(raw, &pretty)
	return enc.Encode(pretty)
}

func printDegraded(w io.Writer, reasons []string) {
	for _, d := range reasons {
		fmt.Fprintf(w, "[degraded: %s]\n", d)
	}
}
