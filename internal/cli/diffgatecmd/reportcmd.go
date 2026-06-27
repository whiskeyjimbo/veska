// SPDX-License-Identifier: AGPL-3.0-only

package diffgatecmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"sort"

	"github.com/whiskeyjimbo/veska/internal/application/blastradius"
	"github.com/whiskeyjimbo/veska/internal/application/diffgate"
	"github.com/whiskeyjimbo/veska/internal/application/staging"
	git "github.com/whiskeyjimbo/veska/internal/infrastructure/git"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
	"github.com/whiskeyjimbo/veska/internal/platform/config"
)

// ReportParams are the advisory PR impact/risk report inputs.
type ReportParams struct {
	RepoID       string
	Branch       string
	RepoRoot     string
	BaseRef      string
	CandidateRef string
	Format       string // json (default) | markdown
	Out          io.Writer
}

// RunReport assembles and emits the advisory PR impact/risk report: the diff's
// blast radius, each changed file's change-risk standing, open findings on the
// touched files, and the changed-but-untested symbols. It is ADVISORY - it never
// gates: presence of findings/risk/untested does NOT cause a non-zero exit, a
// producer that errors degrades its section to a Note, and an un-indexed repo
// yields a noted (still exit-0) report. The ONLY non-zero exit is a usage error
// (missing required flags) - that is an un-runnable invocation, not a gate.
// This deliberately diverges from the gate subcommands, which return
// ErrGateFailed on repo_not_indexed: the report's contract is "drop it in CI and
// it never breaks the build" (the soft on-ramp), so it must survive the most
// common early-adoption state - an index not yet built.
func RunReport(ctx context.Context, p ReportParams) error {
	if p.RepoID == "" || p.BaseRef == "" || p.CandidateRef == "" {
		return fmt.Errorf("diff-gate report: --repo, --base-ref and --candidate-ref are required")
	}

	dbPath := filepath.Join(config.DefaultVectorDir(), "veska.db")
	pools, err := sqlite.OpenPools(dbPath)
	if err != nil {
		return fmt.Errorf("diff-gate report: open store: %w", err)
	}
	defer pools.Close()

	resolved, err := resolveRepoID(ctx, pools.ReadDB, p.RepoID)
	if err != nil {
		return err
	}
	p.RepoID = resolved
	p.Branch = resolveBranch(ctx, pools.ReadDB, p.RepoID, p.Branch)

	report := diffgate.PRReport{
		RepoID:       p.RepoID,
		Branch:       p.Branch,
		BaseRef:      p.BaseRef,
		CandidateRef: p.CandidateRef,
	}

	// Advisory, not a gate: an un-indexed repo yields a noted report and still
	// exits 0 (the other gates fail closed here; the report must not).
	if !repoIndexed(ctx, pools.ReadDB, p.RepoID, p.Branch) {
		report.Notes = append(report.Notes, notIndexedDetail(ctx, pools.ReadDB, p.RepoID))
		return emitReport(p.Out, p.Format, report)
	}

	// One canonical changed-files set drives every section's intersection
	// (repo-relative slash, matching nodes.file_path / git ChangeCounts).
	changedFiles, err := git.ChangedFilesBetween(ctx, p.RepoRoot, p.BaseRef, p.CandidateRef)
	if err != nil {
		report.Notes = append(report.Notes, fmt.Sprintf("changed_files: %v", cleanRefError(err, p.BaseRef, p.CandidateRef)))
		return emitReport(p.Out, p.Format, report)
	}
	report.ChangedFiles = changedFiles

	edges := sqlite.NewEdgeReaderRepo(pools.ReadDB)
	nodes := sqlite.NewNodeLookupRepo(pools.ReadDB)
	blast, err := blastradius.NewService(edges, nodes, staging.NewArea())
	if err != nil {
		report.Notes = append(report.Notes, fmt.Sprintf("blast_radius: service init failed: %v", err))
	}

	// Section 1: blast radius of the diff (downstream impact).
	if blast != nil {
		report.BlastRadius, report.Notes = assembleBlast(ctx, blast, p, changedFiles, report.Notes)
	}
	// Section 2: per-changed-file change-risk standing (freq × blast-radius).
	if blast != nil {
		report.ChangeRisk, report.Notes = assembleChangeRisk(ctx, blast, nodes, p, changedFiles, report.Notes)
	}
	// Section 3: open findings on the touched files.
	report.OpenFindings, report.Notes = assembleFindings(ctx, pools, p, changedFiles, report.Notes)
	// Section 4: changed-but-untested symbols (reuse the coverage machinery,
	// discarding its gating verdict).
	report.Untested, report.Notes = assembleUntested(ctx, pools, dbPath, edges, nodes, p, report.Notes)

	return emitReport(p.Out, p.Format, report)
}

// blastNoiseKinds are node kinds that carry no "downstream impact" signal in the
// PR report: structural containers (package/module/file) and the embedding
// sub-symbol text chunks. They reach the blast set via CONTAINS edges but only
// dilute the symbol-level impact list a reviewer reads. Filtered
// here, in the report consumer, so other blastradius consumers still see them.
var blastNoiseKinds = map[string]struct{}{
	"chunk":   {},
	"package": {},
	"module":  {},
	"file":    {},
}

// filterBlastNoise drops blast entries whose kind carries no downstream-impact
// signal (blastNoiseKinds), preserving order. It returns a fresh slice; the
// input is not mutated.
func filterBlastNoise(entries []blastradius.Entry) []blastradius.Entry {
	out := make([]blastradius.Entry, 0, len(entries))
	for _, e := range entries {
		if _, noise := blastNoiseKinds[e.Kind]; noise {
			continue
		}
		out = append(out, e)
	}
	return out
}

// assembleBlast runs blastradius.DiffOf over the canonical changed-files set and
// projects the response into the report section, dropping non-symbol noise kinds
// (chunk/package/.). Errors degrade to a Note.
func assembleBlast(ctx context.Context, blast *blastradius.Service, p ReportParams, changedFiles []string, notes []string) (diffgate.BlastRadiusSection, []string) {
	changedFunc := func(context.Context, string) ([]string, error) { return changedFiles, nil }
	resp, err := blast.DiffOf(ctx, p.RepoID, p.Branch, p.RepoRoot, changedFunc, blastradius.Options{})
	if err != nil {
		return diffgate.BlastRadiusSection{}, append(notes, fmt.Sprintf("blast_radius: %v", err))
	}
	// Filter out noise kinds first so NodeCount and the entry cap both reflect
	// the symbol-level impact, not container/chunk nodes.
	meaningful := filterBlastNoise(resp.Entries)
	sec := diffgate.BlastRadiusSection{
		SeedFiles: len(changedFiles),
		NodeCount: len(meaningful),
		Truncated: resp.Truncated,
	}
	limit := min(len(meaningful), diffgate.ReportBlastEntryLimit)
	for _, e := range meaningful[:limit] {
		sec.Entries = append(sec.Entries, diffgate.BlastEntry{
			NodeID:     e.NodeID,
			SymbolPath: e.SymbolPath,
			FilePath:   e.FilePath,
			Kind:       e.Kind,
			Distance:   e.Distance,
		})
	}
	return sec, notes
}

// assembleChangeRisk computes each changed file's change-risk directly:
// recent_change_frequency × blast_radius, the same formula wiki.HotZoneService
// ranks by, scoped to the diff's files so every changed file gets a standing
// (no whole-repo top-N truncation). Files are ordered by descending score.
func assembleChangeRisk(ctx context.Context, blast *blastradius.Service, nodes *sqlite.NodeLookupRepo, p ReportParams, changedFiles []string, notes []string) ([]diffgate.ChangeRiskFile, []string) {
	// window 0 == all history, matching the hot_zone surface's composition.
	counts, err := git.ChangeCounts(ctx, p.RepoRoot, 0)
	if err != nil {
		return nil, append(notes, fmt.Sprintf("change_risk: change counts: %v", err))
	}
	out := make([]diffgate.ChangeRiskFile, 0, len(changedFiles))
	for _, f := range changedFiles {
		ids, err := nodes.NodesInFile(ctx, p.RepoID, p.Branch, f)
		if err != nil {
			return nil, append(notes, fmt.Sprintf("change_risk: nodes in %s: %v", f, err))
		}
		radius := 0
		if len(ids) > 0 {
			resp, err := blast.Of(ctx, p.RepoID, p.Branch, ids, blastradius.Options{})
			if err != nil {
				return nil, append(notes, fmt.Sprintf("change_risk: blast %s: %v", f, err))
			}
			radius = len(resp.Entries)
		}
		freq := counts[f]
		out = append(out, diffgate.ChangeRiskFile{
			FilePath:              f,
			RecentChangeFrequency: freq,
			BlastRadius:           radius,
			Score:                 freq * radius,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].FilePath < out[j].FilePath
	})
	return out, notes
}

// assembleFindings lists the open findings whose file is in the diff.
func assembleFindings(ctx context.Context, pools *sqlite.Pools, p ReportParams, changedFiles []string, notes []string) ([]diffgate.ReportFinding, []string) {
	rows, err := sqlite.NewFindingQuerierRepo(pools.ReadDB).OpenFindingsInFiles(ctx, p.RepoID, p.Branch, changedFiles)
	if err != nil {
		return nil, append(notes, fmt.Sprintf("open_findings: %v", err))
	}
	out := make([]diffgate.ReportFinding, 0, len(rows))
	for _, o := range rows {
		out = append(out, diffgate.ReportFinding{
			FindingID: o.FindingID,
			Rule:      o.Rule,
			Severity:  o.Severity,
			FilePath:  o.FilePath,
			NodeID:    o.NodeID,
			Message:   o.Message,
		})
	}
	return out, notes
}

// assembleUntested reuses the diff-coverage machinery (re-promote clone + the
// untested-symbol check) and extracts the changed-but-untested SYMBOLS, throwing
// away the gating verdict (Pass/Failures) so the advisory report never gates.
func assembleUntested(ctx context.Context, pools *sqlite.Pools, dbPath string, edges *sqlite.EdgeReaderRepo, nodes *sqlite.NodeLookupRepo, p ReportParams, notes []string) ([]diffgate.UntestedSymbol, []string) {
	base := baseGraph{EdgeReaderRepo: edges, NodeLookupRepo: nodes}
	eph, changes, err := buildEphemeral(ctx, ephemeralParams{
		RepoID:       p.RepoID,
		Branch:       p.Branch,
		RepoRoot:     p.RepoRoot,
		BaseRef:      p.BaseRef,
		CandidateRef: p.CandidateRef,
	}, base)
	if err != nil {
		return nil, append(notes, fmt.Sprintf("untested_changed: index candidate: %v", err))
	}
	// The report surfaces the symbols as JSON, not SARIF, so the location
	// locator is unused here.
	untested, _, err := untestedInChangedFiles(ctx, pools, dbPath, p.RepoID, p.Branch, p.CandidateRef, changes, eph.ChangedFiles)
	if err != nil {
		return nil, append(notes, fmt.Sprintf("untested_changed: %v", err))
	}
	// Reuse the gate's intersection, then DISCARD its Pass/Failures verdict
	// the report surfaces the symbols, it does not gate on them.
	verdict := diffgate.NewCoverageGate().Evaluate(eph.ChangedNodeIDs(ctx), untested)
	return verdict.UntestedChanged, notes
}

// emitReport writes the report in the requested format: indented JSON (default)
// or a Markdown body for a PR comment. It always returns nil on a successful
// write - the report never signals a gate failure. Every emit site (including
// the degraded not-indexed and changed-files-error paths) routes through here so
// Markdown inherits the soft on-ramp (a Notes-only report still renders).
func emitReport(out io.Writer, format string, report diffgate.PRReport) error {
	if format == "markdown" {
		if _, err := io.WriteString(out, diffgate.RenderMarkdown(report)); err != nil {
			return fmt.Errorf("diff-gate report: write markdown: %w", err)
		}
		return nil
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	if err := enc.Encode(report); err != nil {
		return fmt.Errorf("diff-gate report: encode: %w", err)
	}
	return nil
}
