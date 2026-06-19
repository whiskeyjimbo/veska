// SPDX-License-Identifier: AGPL-3.0-only

package doctorcmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"github.com/whiskeyjimbo/veska/internal/application/review"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/vulnsource/osv"
	"github.com/whiskeyjimbo/veska/internal/platform/config"
	"github.com/whiskeyjimbo/veska/internal/platform/doctor"
	"github.com/whiskeyjimbo/veska/internal/platform/health"
)

// StubOK prints an "ok" message for stub subcommands that have no real probe yet.
func StubOK(subsystem string, jsonOut bool, w io.Writer) error {
	if jsonOut {
		return json.NewEncoder(w).Encode(doctor.NewEnvelope(subsystem, health.StatusHealthy, map[string]any{}))
	}
	fmt.Fprintf(w, "%s: ok\n", subsystem)
	return nil
}

// QueueOptions are the boolean flags for RunPostPromotionQueue. Probes that
// carry a single flag keep it positional (e.g. RunEgress(w, jsonOut)); those
// with two or more take an options struct so adjacent bools can't be
// transposed at the call site.
type QueueOptions struct {
	JSON         bool
	PurgeOrphans bool
}

// RunPostPromotionQueue inspects the post-promotion queue depth and failed
// rows, optionally purging orphan failed rows first.
func RunPostPromotionQueue(w io.Writer, opts QueueOptions) error {
	jsonOut, purgeOrphans := opts.JSON, opts.PurgeOrphans
	dbPath := filepath.Join(config.DefaultVectorDir(), "veska.db")
	// purge-orphans deletes failed rows whose repo_id
	// was deregistered. Without this, removed-repo rows linger
	// forever and drag the rollup to "degraded".
	if purgeOrphans {
		n, err := doctor.PurgeOrphanFailedRows(dbPath)
		if err != nil {
			return fmt.Errorf("purge orphan failed rows: %w", err)
		}
		fmt.Fprintf(w, "purged %d orphan failed row(s) (repo_id no longer registered)\n", n)
	}
	report, err := doctor.CheckPostPromotionQueue(dbPath)
	if err != nil {
		return err
	}
	if jsonOut {
		return json.NewEncoder(w).Encode(doctor.NewEnvelope("post_promotion_queue", report.Status, report))
	}
	fmt.Fprintf(w, "post_promotion_queue: %s (state_counts=%d, failed=%d)\n",
		report.Status, len(report.Counts), len(report.FailedRows))
	for _, c := range report.Counts {
		fmt.Fprintf(w, "  %s/%s: %d\n", c.State, c.WorkKind, c.Count)
	}
	for _, f := range report.FailedRows {
		fmt.Fprintf(w, "  FAILED seq=%d repo=%s branch=%s kind=%s attempts=%d err=%s\n",
			f.Seq, f.RepoID, f.Branch, f.WorkKind, f.Attempts, f.Error)
	}
	// when the failed set includes rows pointing at
	// deregistered repos, tell the operator what to run instead of
	// leaving them to grep the error messages for "is not registered".
	if report.OrphanCount > 0 {
		fmt.Fprintf(w, "  hint: %d failed row(s) point at a deregistered repo - run `veska doctor post_promotion_queue --purge-orphans` to clear them\n", report.OrphanCount)
	}
	if report.Status != "healthy" {
		return ProbeStatusError{Subsystem: "post_promotion_queue", Status: string(report.Status)}
	}
	return nil
}

// RunIdentity reports each registered repo's resolved identity tier and warns
// when any repo sits on a non-converging tier - its node_ids would not match
// another contributor indexing the same upstream in a shared graph DB
// Non-converging is the expected, fine state for single-user use,
// so this probe is advisory: it is NOT folded into the `doctor status` rollup.
func RunIdentity(w io.Writer, jsonOut bool) error {
	dbPath := filepath.Join(config.DefaultVectorDir(), "veska.db")
	report, err := doctor.CheckIdentityTiers(dbPath)
	if err != nil {
		return err
	}
	if jsonOut {
		return json.NewEncoder(w).Encode(doctor.NewEnvelope("identity", report.Status, report))
	}
	fmt.Fprintf(w, "identity: %s (%d repo(s), %d non-converging)\n",
		report.Status, len(report.Repos), report.NonConverging)
	for _, r := range report.Repos {
		short := r.RepoID
		if len(short) > 12 {
			short = short[:12]
		}
		note := ""
		if !r.Converges {
			// Distinguish an unresolved (pre-0018) repo from a resolved-but
			// local-only tier; they read differently to an operator.
			if r.Tier == "" {
				note = "  WARN unresolved tier - won't converge in a shared DB"
			} else {
				note = "  WARN won't converge in a shared DB"
			}
		}
		tier := r.Tier
		if tier == "" {
			tier = "(unresolved)"
		}
		fmt.Fprintf(w, "  %s tier=%s%s\n", short, tier, note)
	}
	// Legend on every run: the raw tier names are opaque on the
	// healthy path, where the non-converging hint below never fires. A junior
	// seeing `tier=module-hostpath` needs to know it's the good one.
	if len(report.Repos) > 0 {
		fmt.Fprintln(w, "  tiers: module-hostpath converges (shareable across contributors); origin-url/module-bare/abs-root are local-only")
	}
	if report.NonConverging > 0 {
		fmt.Fprintf(w, "  hint: only the module-hostpath tier (go.mod `github.com/org/repo`) converges; see ADR-S0017\n")
	}
	if report.Status != "healthy" {
		return ProbeStatusError{Subsystem: "identity", Status: string(report.Status)}
	}
	return nil
}

// RunWikiRender reports the age of the last successful wiki render.
func RunWikiRender(ctx context.Context, w io.Writer, jsonOut bool) error {
	dbPath := filepath.Join(config.DefaultVectorDir(), "veska.db")

	pools, err := sqlite.OpenPools(dbPath)
	if err != nil {
		return fmt.Errorf("wiki_render: open sqlite pools: %w", err)
	}
	defer func() { _ = pools.Close() }()

	store := sqlite.NewWikiRenderStateRepo(pools.ReadDB, pools.Write)
	report, err := doctor.CheckWikiRender(ctx, store)
	if err != nil {
		return err
	}
	if jsonOut {
		return json.NewEncoder(w).Encode(doctor.NewEnvelope("wiki_render", report.Status, report))
	}
	switch {
	case report.Status != "healthy":
		fmt.Fprintf(w, "wiki_render: %s\n", report.Status)
	case !report.Rendered:
		fmt.Fprintf(w, "wiki_render: %s (never rendered)\n", report.Status)
	default:
		fmt.Fprintf(w, "wiki_render: %s (last_render_at=%s, age=%s)\n",
			report.Status, report.LastRenderAt.Format(time.RFC3339),
			(time.Duration(report.AgeSeconds) * time.Second))
	}
	if report.Status != "healthy" {
		return ProbeStatusError{Subsystem: "wiki_render", Status: string(report.Status)}
	}
	return nil
}

// RunPipelines reports review-pipeline token usage against the configured caps.
func RunPipelines(ctx context.Context, w io.Writer, jsonOut bool) error {
	fileCfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("pipelines: load config: %w", err)
	}

	dbPath := filepath.Join(config.DefaultVectorDir(), "veska.db")
	pools, err := sqlite.OpenPools(dbPath)
	if err != nil {
		return fmt.Errorf("pipelines: open sqlite pools: %w", err)
	}
	defer func() { _ = pools.Close() }()

	tokenStore := sqlite.NewReviewTokenStore(pools.ReadDB, pools.Write)
	quota := review.NewQuota(
		fileCfg.Review.MaxTokensPerCommit,
		fileCfg.Review.MaxTokensPerDay,
		tokenStore)

	report, err := doctor.CheckPipelines(ctx, quota,
		fileCfg.Review.MaxTokensPerDay, fileCfg.Review.MaxTokensPerCommit)
	if err != nil {
		return err
	}
	if jsonOut {
		return json.NewEncoder(w).Encode(doctor.NewEnvelope("pipelines", report.Status, report))
	}
	fmt.Fprintf(w, "pipelines: %s (tokens_today=%d, max_per_day=%d, max_per_commit=%d, paused=%v)\n",
		report.Status, report.TokensToday, report.MaxTokensPerDay,
		report.MaxTokensPerCommit, report.Paused)
	if report.Status != "healthy" {
		return ProbeStatusError{Subsystem: "pipelines", Status: string(report.Status)}
	}
	return nil
}

// RunEmbedder verifies the embedder the daemon actually elected - in-process
// by default, Ollama only when VESKA_EMBEDDER=ollama.
func RunEmbedder(w io.Writer, jsonOut bool) error {
	h := CheckEmbedderHealth(context.Background(), config.DefaultVectorDir())
	if jsonOut {
		if h.Probe != nil {
			return json.NewEncoder(w).Encode(doctor.NewEnvelope("embedder", h.Status, h.Probe))
		}
		return json.NewEncoder(w).Encode(doctor.NewEnvelope("embedder", h.Status, map[string]any{"detail": h.Detail}))
	}
	fmt.Fprintf(w, "embedder: %s (%s)\n", h.Status, h.Detail)
	if h.Probe != nil && h.Probe.InstallHint != "" && h.Status != "healthy" {
		fmt.Fprintf(w, "  hint: %s\n", h.Probe.InstallHint)
	}
	if h.Status != "healthy" {
		return ProbeStatusError{Subsystem: "embedder", Status: string(h.Status)}
	}
	return nil
}

// egressObservabilityParams builds the observability egress report inputs from
// config. Each destination is reported only when its feature is enabled.
func egressObservabilityParams(cfg config.Config) doctor.EgressObservabilityParams {
	obsParams := doctor.EgressObservabilityParams{}
	if cfg.Metrics.Enabled {
		obsParams.MetricsListener = cfg.Metrics.Listen
		obsParams.MetricsConfiguredVia = "config:metrics.listen"
	}
	if cfg.Tracing.Enabled {
		obsParams.OTLPEndpoint = cfg.Tracing.OTLPEndpoint
		obsParams.OTLPConfiguredVia = "config:tracing.otlp_endpoint"
	}
	if cfg.Review.Enabled {
		obsParams.ReviewLLMEndpoint = cfg.LLMGenerator.Endpoint
		obsParams.ReviewLLMConfiguredVia = "config:llm_generator.endpoint"
	}
	// The OSV advisory dump is reported only when [vuln_source] is
	// configured with the osv provider (the feature is off by default).
	if cfg.VulnSource.Provider == "osv" {
		obsParams.VulnSourceEndpoint = osv.DumpURL
		obsParams.VulnSourceConfiguredVia = "config:vuln_source.provider"
	}
	return obsParams
}

// RunEgress verifies daemon socket and control-plane connectivity plus the
// configured observability egress destinations.
func RunEgress(w io.Writer, jsonOut bool) error {
	report, err := doctor.CheckEgress([]string{
		config.CLISockPath(),
		config.MCPSockPath(),
	})
	if err != nil {
		return err
	}
	// Compute egress status.
	egressStatus := health.StatusHealthy
	for _, s := range report.Sockets {
		if s.Status == "missing" {
			egressStatus = health.StatusBroken
			break
		}
	}

	// Build the observability egress report from config. The review
	// LLM endpoint is reported only when the review pipeline is
	// enabled (passing "" otherwise omits the destination).
	cfg, _ := config.Load()
	obsReport := doctor.CheckEgressObservability(egressObservabilityParams(cfg))

	if jsonOut {
		envelope := struct {
			doctor.EgressReport
			Observability doctor.EgressObservabilityReport `json:"observability"`
		}{EgressReport: report, Observability: obsReport}
		return json.NewEncoder(w).Encode(doctor.NewEnvelope("egress", egressStatus, envelope))
	}
	anyMissing := false
	for _, s := range report.Sockets {
		fmt.Fprintf(w, "egress: %s (%s)\n", s.Status, s.Path)
		if s.Status == "missing" {
			anyMissing = true
		}
	}
	for _, d := range obsReport.Destinations {
		target := d.URL
		if target == "" {
			target = d.Listen
		}
		fmt.Fprintf(w, "egress: %s -> %s (%s)\n", d.Kind, target, d.ConfiguredVia)
	}
	if anyMissing {
		return ProbeStatusError{Subsystem: "egress", Status: "broken"}
	}
	return nil
}

// RunConfig validates veska configuration values.
func RunConfig(w io.Writer, jsonOut bool) error {
	report, err := doctor.CheckConfig(config.DefaultVectorDir())
	if err != nil {
		return err
	}
	// Compute config status.
	configStatus := health.StatusHealthy
	if !report.DBExists {
		configStatus = health.StatusDegraded
	}
	if jsonOut {
		return json.NewEncoder(w).Encode(doctor.NewEnvelope("config", configStatus, report))
	}
	fmt.Fprintf(w, "config: veska_home=%s db_exists=%v veska_home_set=%v\n",
		report.VeskaHome, report.DBExists, report.VeskaHomeSet)
	// surface the ephemeral-cache knobs so the
	// user can see effective values and where each came from
	// (default vs env override) without grepping the source.
	fmt.Fprintf(w, "cache: declined_ttl=%s (%s) max_bytes=%d (%s) max_ephemerals=%d (%s)\n",
		report.Cache.DeclinedTTL, report.Cache.DeclinedTTLSource,
		report.Cache.MaxBytes, report.Cache.MaxBytesSource,
		report.Cache.MaxEphemerals, report.Cache.MaxEphemeralsSource)
	if report.CacheError != "" {
		fmt.Fprintf(w, "cache: WARNING %s\n", report.CacheError)
	}
	if !report.DBExists {
		return ProbeStatusError{Subsystem: "config", Status: "degraded"}
	}
	return nil
}

// RunService checks supervisor state and broken-marker presence.
func RunService(w io.Writer, jsonOut bool) error {
	home := config.DefaultVectorDir()
	report, err := doctor.CheckService(home)
	if err != nil {
		return err
	}
	if jsonOut {
		return json.NewEncoder(w).Encode(doctor.NewEnvelope("service", report.Status, report))
	}
	fmt.Fprintf(w, "service: %s (daemon_running=%v, broken_marker=%v)\n",
		report.Status, report.DaemonRunning, report.BrokenMarkerPresent)
	if report.BrokenMarkerPresent {
		fmt.Fprintf(w, "  broken marker: %s\n", report.BrokenMarkerPath)
	}
	if report.Status != "healthy" {
		return ProbeStatusError{Subsystem: "service", Status: string(report.Status)}
	}
	return nil
}

// RunBackup verifies the most recent backup archive and reports its age.
// backupDirExists reports whether dir contains at least one backup tarball;
// the cmd package injects it (shared with `veska restore`) so doctorcmd does
// not re-implement the legacy-dir fallback scan.
func RunBackup(w io.Writer, jsonOut bool, backupDirExists func(string) bool) error {
	// prefer the canonical $VESKA_HOME/backups; fall
	// back to legacy ~/.veska-backups so doctor doesn't report
	// "no backups" right after an upgrade that hasn't run a new
	// backup yet.
	backupDir := config.DefaultBackupDir()
	if !backupDirExists(backupDir) {
		if legacy, ok := config.LegacyBackupDir(); ok && backupDirExists(legacy) {
			backupDir = legacy
		}
	}
	report, err := doctor.CheckBackup(backupDir)
	if err != nil {
		return err
	}
	if jsonOut {
		return json.NewEncoder(w).Encode(doctor.NewEnvelope("backup", report.Status, report))
	}
	switch report.Status {
	case "healthy":
		fmt.Fprintf(w, "backup: %s (latest=%s, age_hours=%.2f, count=%d)\n",
			report.Status, filepath.Base(report.LatestFile), report.AgeHours, report.FileCount)
	case "degraded":
		fmt.Fprintf(w, "backup: %s (no .tar.gz files found in %s)\n",
			report.Status, report.BackupDir)
	case "broken":
		fmt.Fprintf(w, "backup: %s (latest=%s, verify_error=%s)\n",
			report.Status, filepath.Base(report.LatestFile), report.VerifyError)
	}
	if report.Status != "healthy" {
		return ProbeStatusError{Subsystem: "backup", Status: string(report.Status)}
	}
	return nil
}

// RunResetCrashLoop removes the broken marker and crash-count files so the
// daemon can start after a crash-loop trip.
func RunResetCrashLoop(w io.Writer, jsonOut bool) error {
	home := config.DefaultVectorDir()
	report, err := doctor.ResetCrashLoop(home)
	if err != nil {
		return err
	}
	if jsonOut {
		return json.NewEncoder(w).Encode(report)
	}
	if report.BrokenMarkerCleared {
		fmt.Fprintln(w, "cleared broken marker")
	} else {
		fmt.Fprintln(w, "broken marker not present (nothing to clear)")
	}
	if report.CrashCountCleared {
		fmt.Fprintf(w, "cleared crash count (was %d)\n", report.CrashCountWas)
	} else {
		fmt.Fprintln(w, "crash count not present (nothing to clear)")
	}
	return nil
}

// RunStorage reports filesystem storage metrics for the veska data directory.
func RunStorage(w io.Writer, jsonOut bool) error {
	report, err := doctor.CheckStorage(config.DefaultVectorDir())
	if err != nil {
		return err
	}
	if jsonOut {
		return json.NewEncoder(w).Encode(doctor.NewEnvelope("storage", health.StatusHealthy, report))
	}
	fmt.Fprintf(w, "storage: ok (db=%d bytes, wal=%d bytes, hnsw=%d bytes, free_ratio=%.2f)\n",
		report.DBSizeBytes, report.WALSizeBytes, report.HNSWSizeBytes, report.FreeRatio)
	return nil
}

// RunBundle writes a diagnostic tarball (manifest, all probe outputs, redacted
// audit tail) and prints the resulting path.
func RunBundle(w io.Writer, jsonOut bool, outputDir string) error {
	result, err := doctor.CreateBundle(doctor.BundleOptions{
		VeskaHome: config.DefaultVectorDir(),
		OutputDir: outputDir,
		OllamaURL: DefaultOllamaURL,
		ModelName: DefaultModelName,
	})
	if err != nil {
		return err
	}
	if jsonOut {
		return json.NewEncoder(w).Encode(doctor.NewEnvelope("bundle", health.StatusHealthy, map[string]any{
			"path":       result.Path,
			"file_count": result.FileCount,
		}))
	}
	fmt.Fprintln(w, result.Path)
	fmt.Fprintln(w, "attach this tarball to support / issue reports - contains probe outputs and recent audit log")
	return nil
}
