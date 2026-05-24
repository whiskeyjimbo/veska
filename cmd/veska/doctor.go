package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/whiskeyjimbo/veska/internal/application/review"
	"github.com/whiskeyjimbo/veska/internal/config"
	"github.com/whiskeyjimbo/veska/internal/doctor"
	"github.com/whiskeyjimbo/veska/internal/embedderprobe"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/embedding/elect"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/vulnsource/osv"
	"github.com/whiskeyjimbo/veska/internal/repo"
)

const (
	defaultOllamaURL = "http://localhost:11434"
	defaultModelName = "nomic-embed-text"
)

// ProbeStatusError is returned by doctor subcommands when a probe yields a
// non-healthy status.  main() translates it to the appropriate OS exit code.
type ProbeStatusError struct {
	Subsystem string
	Status    string // "degraded" or "broken"
}

func (e ProbeStatusError) Error() string {
	return e.Subsystem + ": " + e.Status
}

// isProbeStatusError reports whether err is a ProbeStatusError and,
// if so, sets *out to its value.
func isProbeStatusError(err error, out *ProbeStatusError) bool {
	if err == nil {
		return false
	}
	p, ok := err.(ProbeStatusError)
	if ok {
		*out = p
	}
	return ok
}

// exitCodeForProbeStatus returns the conventional exit code for a probe status.
//
//	healthy  → 0
//	degraded → 0 (informational; the human-readable line is still printed to stderr)
//	broken   → 2
//
// Treating "degraded" as a non-failure keeps CI pipelines green when veska is
// merely in a transient warning state (e.g. a single unindexed repo, embedder
// warming up). Callers that want strict gating can re-introduce failure
// downstream by grepping the textual output or by parsing `--json` envelopes.
func exitCodeForProbeStatus(status string) int {
	if status == "broken" {
		return 2
	}
	return 0
}

// doctorCmd returns the "doctor" Cobra command with health-check subcommands.
// Exit codes:
//
//	0 = healthy or degraded (degraded is informational; check stderr for detail)
//	2 = broken
func doctorCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "doctor",
		Short:        "Health checks for the veska runtime",
		SilenceUsage: true,
	}

	cmd.AddCommand(
		doctorStatusCmd(),
		doctorEgressCmd(),
		doctorStorageCmd(),
		doctorEmbedderCmd(),
		doctorConfigCmd(),
		doctorPostPromotionQueueCmd(),
		doctorWikiRenderCmd(),
		doctorServiceCmd(),
		doctorPipelinesCmd(),
		doctorBundleCmd(),
		doctorBackupCmd(),
		doctorResetCrashLoopCmd(),
		doctorSavingsCmd(),
	)

	return cmd
}

// doctorPostPromotionQueueCmd returns the "doctor post_promotion_queue" subcommand
// backed by internal/doctor.CheckPostPromotionQueue.
func doctorPostPromotionQueueCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:          "post_promotion_queue",
		Short:        "Inspect the post-promotion queue depth and failed rows",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			w := cmd.OutOrStdout()
			dbPath := filepath.Join(config.DefaultVectorDir(), "veska.db")
			report, err := doctor.CheckPostPromotionQueue(dbPath)
			if err != nil {
				return err
			}
			if jsonOut {
				enc := json.NewEncoder(w)
				return enc.Encode(doctor.NewEnvelope("post_promotion_queue", report.Status, report))
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
			if report.Status != "healthy" {
				return ProbeStatusError{Subsystem: "post_promotion_queue", Status: report.Status}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "output results as JSON")
	return cmd
}

// doctorWikiRenderCmd returns the "doctor wiki_render" subcommand backed by
// internal/doctor.CheckWikiRender. It reports the age of the last successful
// wiki render, or that no render has occurred yet (which is not an error).
func doctorWikiRenderCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:          "wiki_render",
		Short:        "Report the age of the last successful wiki render",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			w := cmd.OutOrStdout()
			dbPath := filepath.Join(config.DefaultVectorDir(), "veska.db")

			pools, err := sqlite.OpenPools(dbPath)
			if err != nil {
				return fmt.Errorf("wiki_render: open sqlite pools: %w", err)
			}
			defer func() { _ = pools.Close() }()

			store := sqlite.NewWikiRenderStateRepo(pools.ReadDB, pools.WriteHot)
			report, err := doctor.CheckWikiRender(cmd.Context(), store)
			if err != nil {
				return err
			}
			if jsonOut {
				enc := json.NewEncoder(w)
				return enc.Encode(doctor.NewEnvelope("wiki_render", report.Status, report))
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
				return ProbeStatusError{Subsystem: "wiki_render", Status: report.Status}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "output results as JSON")
	return cmd
}

// doctorPipelinesCmd returns the "doctor pipelines" subcommand backed by
// internal/doctor.CheckPipelines. It reports the review pipeline's cumulative
// token usage for the current local day against the configured caps. A
// degraded status means the per-day cap is reached and the review pipeline is
// paused until the local-midnight window reset.
func doctorPipelinesCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:          "pipelines",
		Short:        "Report review-pipeline token usage against the configured caps",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			w := cmd.OutOrStdout()
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

			tokenStore := sqlite.NewReviewTokenStore(pools.ReadDB, pools.WriteHot)
			quota := review.NewQuota(
				fileCfg.Review.MaxTokensPerCommit,
				fileCfg.Review.MaxTokensPerDay,
				tokenStore, nil)

			report, err := doctor.CheckPipelines(cmd.Context(), quota,
				fileCfg.Review.MaxTokensPerDay, fileCfg.Review.MaxTokensPerCommit)
			if err != nil {
				return err
			}
			if jsonOut {
				enc := json.NewEncoder(w)
				return enc.Encode(doctor.NewEnvelope("pipelines", report.Status, report))
			}
			fmt.Fprintf(w, "pipelines: %s (tokens_today=%d, max_per_day=%d, max_per_commit=%d, paused=%v)\n",
				report.Status, report.TokensToday, report.MaxTokensPerDay,
				report.MaxTokensPerCommit, report.Paused)
			if report.Status != "healthy" {
				return ProbeStatusError{Subsystem: "pipelines", Status: report.Status}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "output results as JSON")
	return cmd
}

// stubOK prints an "ok" message for stub subcommands that have no real probe yet.
func stubOK(subsystem string, jsonOut bool, w io.Writer) error {
	if jsonOut {
		enc := json.NewEncoder(w)
		return enc.Encode(doctor.NewEnvelope(subsystem, "healthy", map[string]any{}))
	}
	fmt.Fprintf(w, "%s: ok\n", subsystem)
	return nil
}

// doctorSubCmd creates a generic stub doctor subcommand with a --json flag.
func doctorSubCmd(use, short string, run func(bool, io.Writer) error) *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:          use,
		Short:        short,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return run(jsonOut, cmd.OutOrStdout())
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "output results as JSON")
	return cmd
}

// embedderHealth describes the elected embedder's health for doctor output.
//
// The elected embedder — not Ollama unconditionally — is what doctor must
// report. For the in-process embedders (model2vec / static-v2) the provider
// is constructed locally and is healthy whenever election succeeds; no network
// probe applies. Only VESKA_EMBEDDER=ollama probes a remote Ollama instance.
type embedderHealth struct {
	Status string                     // healthy | degraded | broken
	Detail string                     // human-readable one-liner
	Probe  *embedderprobe.ProbeResult // non-nil only on the ollama path
}

// checkEmbedderHealth resolves the elected embedder the same way the daemon
// and `veska init` do, and reports its health. It mirrors init's
// resolveInitEmbedder so the two never disagree about which embedder is live.
func checkEmbedderHealth(ctx context.Context, home string) embedderHealth {
	override := os.Getenv("VESKA_EMBEDDER")
	if strings.EqualFold(override, elect.OverrideOllama) {
		url := envOrDefault("VESKA_OLLAMA_URL", defaultOllamaURL)
		model := envOrDefault("VESKA_EMBED_MODEL", defaultModelName)
		res, err := embedderprobe.Probe(ctx, url, model)
		if err != nil {
			return embedderHealth{Status: "broken", Detail: fmt.Sprintf("ollama probe error: %v", err)}
		}
		return embedderHealth{
			Status: res.Status,
			Detail: fmt.Sprintf("ollama %s @ %s (reachable=%v, model_present=%v, embed_ok=%v)",
				model, url, res.Reachable, res.ModelPresent, res.EmbedOK),
			Probe: res,
		}
	}
	prov, err := elect.Resolve(elect.Config{VeskaHome: home, Override: override})
	if err != nil {
		return embedderHealth{Status: "broken", Detail: fmt.Sprintf("election failed: %v", err)}
	}
	return embedderHealth{Status: "healthy", Detail: prov.ModelID() + ", in-process"}
}

// doctorEmbedderCmd returns the "doctor embedder" subcommand. It verifies the
// embedder the daemon actually elected — in-process by default, Ollama only
// when VESKA_EMBEDDER=ollama.
func doctorEmbedderCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:          "embedder",
		Short:        "Verify the elected embedding provider",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			w := cmd.OutOrStdout()
			h := checkEmbedderHealth(context.Background(), config.DefaultVectorDir())
			if jsonOut {
				enc := json.NewEncoder(w)
				if h.Probe != nil {
					return enc.Encode(doctor.NewEnvelope("embedder", h.Status, h.Probe))
				}
				return enc.Encode(doctor.NewEnvelope("embedder", h.Status, map[string]any{"detail": h.Detail}))
			}
			fmt.Fprintf(w, "embedder: %s (%s)\n", h.Status, h.Detail)
			if h.Probe != nil && h.Probe.InstallHint != "" && h.Status != "healthy" {
				fmt.Fprintf(w, "  hint: %s\n", h.Probe.InstallHint)
			}
			if h.Status != "healthy" {
				return ProbeStatusError{Subsystem: "embedder", Status: h.Status}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "output results as JSON")
	return cmd
}

// doctorEgressCmd returns the "doctor egress" subcommand backed by internal/doctor.CheckEgress.
func doctorEgressCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:          "egress",
		Short:        "Verify daemon socket and control-plane connectivity",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			w := cmd.OutOrStdout()
			sockPaths := []string{
				config.CLISockPath(),
				config.MCPSockPath(),
			}
			report, err := doctor.CheckEgress(sockPaths)
			if err != nil {
				return err
			}
			// Compute egress status.
			egressStatus := "healthy"
			for _, s := range report.Sockets {
				if s.Status == "missing" {
					egressStatus = "broken"
					break
				}
			}

			// Build the observability egress report from config. The review
			// LLM endpoint is reported only when the review pipeline is
			// enabled (passing "" otherwise omits the destination).
			cfg, _ := config.Load()
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
			obsReport := doctor.CheckEgressObservability(obsParams)

			if jsonOut {
				enc := json.NewEncoder(w)
				envelope := struct {
					doctor.EgressReport
					Observability doctor.EgressObservabilityReport `json:"observability"`
				}{EgressReport: report, Observability: obsReport}
				return enc.Encode(doctor.NewEnvelope("egress", egressStatus, envelope))
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
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "output results as JSON")
	return cmd
}

// doctorConfigCmd returns the "doctor config" subcommand backed by internal/doctor.CheckConfig.
func doctorConfigCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:          "config",
		Short:        "Validate veska configuration values",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			w := cmd.OutOrStdout()
			report, err := doctor.CheckConfig(config.DefaultVectorDir())
			if err != nil {
				return err
			}
			// Compute config status.
			configStatus := "healthy"
			if !report.DBExists {
				configStatus = "degraded"
			}
			if jsonOut {
				enc := json.NewEncoder(w)
				return enc.Encode(doctor.NewEnvelope("config", configStatus, report))
			}
			fmt.Fprintf(w, "config: veska_home=%s db_exists=%v veska_home_set=%v\n",
				report.VeskaHome, report.DBExists, report.VeskaHomeSet)
			if !report.DBExists {
				return ProbeStatusError{Subsystem: "config", Status: "degraded"}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "output results as JSON")
	return cmd
}

// doctorStatusCmd returns the "doctor status" subcommand that rolls up all probes.
func doctorStatusCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:          "status",
		Short:        "Overall health rollup across all subsystems",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			home := config.DefaultVectorDir()

			embedderResult := checkEmbedderHealth(context.Background(), home)
			egressReport, _ := doctor.CheckEgress([]string{
				config.CLISockPath(),
				config.MCPSockPath(),
			})
			configReport, _ := doctor.CheckConfig(home)
			storageReport, _ := doctor.CheckStorage(home)
			ingestionStatus, ingestionDetail := checkIngestion(context.Background())

			// Compute egress status: broken if any socket is missing.
			egressStatus := "healthy"
			for _, s := range egressReport.Sockets {
				if s.Status == "missing" {
					egressStatus = "broken"
					break
				}
			}

			// Compute config status.
			configStatus := "healthy"
			if !configReport.DBExists {
				configStatus = "degraded"
			}

			// Storage is always healthy (no failure mode currently).
			_ = storageReport

			// Roll up: broken if any broken; degraded if any degraded.
			statuses := []string{embedderResult.Status, egressStatus, configStatus, ingestionStatus}
			rollup := "healthy"
			for _, s := range statuses {
				switch s {
				case "broken":
					rollup = "broken"
				case "degraded":
					if rollup != "broken" {
						rollup = "degraded"
					}
				}
			}

			if jsonOut {
				type statusRollupData struct {
					Embedder        string `json:"embedder"`
					Egress          string `json:"egress"`
					Config          string `json:"config"`
					Ingestion       string `json:"ingestion"`
					IngestionDetail string `json:"ingestion_detail,omitempty"`
				}
				enc := json.NewEncoder(cmd.OutOrStdout())
				return enc.Encode(doctor.NewEnvelope("status", rollup, statusRollupData{
					Embedder:        embedderResult.Status,
					Egress:          egressStatus,
					Config:          configStatus,
					Ingestion:       ingestionStatus,
					IngestionDetail: ingestionDetail,
				}))
			}
			detail := ""
			if ingestionDetail != "" {
				detail = " — " + ingestionDetail
			}
			fmt.Fprintf(cmd.OutOrStdout(), "status: %s (embedder=%s, egress=%s, config=%s, ingestion=%s)%s\n",
				rollup, embedderResult.Status, egressStatus, configStatus, ingestionStatus, detail)
			if rollup != "healthy" {
				return ProbeStatusError{Subsystem: "status", Status: rollup}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "output results as JSON")
	return cmd
}

// checkIngestion inspects the repos table for never-promoted entries
// (last_promoted_sha IS NULL or ”). A repo that has been registered
// but is still unindexed is real degraded state — the daemon either is
// not running, is mid-cold-scan, or hit a per-repo failure during
// startup-resync (solov2-8ga's continue-on-error path) — and 'doctor
// status' should not report 'healthy' while that's true (solov2-b9y).
//
// Returns ("healthy"|"degraded", detail). detail is "" when healthy.
// Database open errors are reported as 'degraded' with the err message
// so the user gets a hint rather than a silent miss.
func checkIngestion(ctx context.Context) (string, string) {
	db, closeFn, err := openLocalDB()
	if err != nil {
		return "degraded", fmt.Sprintf("repos db unreadable: %v", err)
	}
	defer closeFn()

	recs, err := repo.List(ctx, db)
	if err != nil {
		return "degraded", fmt.Sprintf("repos list failed: %v", err)
	}
	if len(recs) == 0 {
		return "healthy", ""
	}
	var unindexed []string
	for _, r := range recs {
		if r.LastPromotedSHA == "" {
			short := r.RepoID
			if len(short) > 12 {
				short = short[:12]
			}
			unindexed = append(unindexed, short)
		}
	}
	if len(unindexed) == 0 {
		return "healthy", ""
	}
	return "degraded", fmt.Sprintf("%d unindexed repo(s): %v", len(unindexed), unindexed)
}

// doctorServiceCmd returns the "doctor service" subcommand backed by internal/doctor.CheckService.
// Exit codes follow SOLO-13 §2.1:
//
//	0 = healthy  (daemon running, no broken marker)
//	1 = degraded (daemon unreachable, no broken marker)
//	2 = broken   (broken marker present)
func doctorServiceCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:          "service",
		Short:        "Check supervisor state and broken-marker presence",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			w := cmd.OutOrStdout()
			home := config.DefaultVectorDir()
			report, err := doctor.CheckService(home)
			if err != nil {
				return err
			}
			if jsonOut {
				enc := json.NewEncoder(w)
				return enc.Encode(doctor.NewEnvelope("service", report.Status, report))
			}
			fmt.Fprintf(w, "service: %s (daemon_running=%v, broken_marker=%v)\n",
				report.Status, report.DaemonRunning, report.BrokenMarkerPresent)
			if report.BrokenMarkerPresent {
				fmt.Fprintf(w, "  broken marker: %s\n", report.BrokenMarkerPath)
			}
			if report.Status != "healthy" {
				return ProbeStatusError{Subsystem: "service", Status: report.Status}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "output results as JSON")
	return cmd
}

// doctorBackupCmd returns the "doctor backup" subcommand backed by internal/doctor.CheckBackup.
// Exit codes follow SOLO-13 §2.1:
//
//	0 = healthy  (most recent .tar.gz exists and passes gzip verification)
//	1 = degraded (no backup files found)
//	2 = broken   (most recent backup fails gzip verification)
func doctorBackupCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:          "backup",
		Short:        "Verify most recent backup archive and report its age",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			w := cmd.OutOrStdout()
			homeDir, err := os.UserHomeDir()
			if err != nil {
				return err
			}
			backupDir := filepath.Join(homeDir, ".veska-backups")
			report, err := doctor.CheckBackup(backupDir)
			if err != nil {
				return err
			}
			if jsonOut {
				enc := json.NewEncoder(w)
				return enc.Encode(doctor.NewEnvelope("backup", report.Status, report))
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
				return ProbeStatusError{Subsystem: "backup", Status: report.Status}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "output results as JSON")
	return cmd
}

// doctorResetCrashLoopCmd returns the "doctor reset-crash-loop" subcommand that
// removes the broken marker and crash-count files from the veska home directory,
// allowing the daemon to start after a crash-loop trip.
func doctorResetCrashLoopCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:          "reset-crash-loop",
		Short:        "Clear broken marker and restart counter so the daemon can start again",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			w := cmd.OutOrStdout()
			home := config.DefaultVectorDir()
			report, err := doctor.ResetCrashLoop(home)
			if err != nil {
				return err
			}
			if jsonOut {
				enc := json.NewEncoder(w)
				return enc.Encode(report)
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
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "output results as JSON")
	return cmd
}

// doctorStorageCmd returns the "doctor storage" subcommand backed by internal/doctor.CheckStorage.
func doctorStorageCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:          "storage",
		Short:        "Report filesystem storage metrics for the veska data directory",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			w := cmd.OutOrStdout()
			report, err := doctor.CheckStorage(config.DefaultVectorDir())
			if err != nil {
				return err
			}
			if jsonOut {
				enc := json.NewEncoder(w)
				return enc.Encode(doctor.NewEnvelope("storage", "healthy", report))
			}
			fmt.Fprintf(w, "storage: ok (db=%d bytes, wal=%d bytes, hnsw=%d bytes, free_ratio=%.2f)\n",
				report.DBSizeBytes, report.WALSizeBytes, report.HNSWSizeBytes, report.FreeRatio)
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "output results as JSON")
	return cmd
}

// doctorBundleCmd returns the "doctor bundle" subcommand that writes a diagnostic
// tarball (manifest, all probe outputs, redacted audit tail) to a temp directory
// and prints the resulting path.
func doctorBundleCmd() *cobra.Command {
	var outputDir string
	var jsonOut bool
	cmd := &cobra.Command{
		Use:          "bundle",
		Short:        "Write a diagnostic tarball with all probe outputs and audit tail",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			w := cmd.OutOrStdout()
			result, err := doctor.CreateBundle(doctor.BundleOptions{
				VeskaHome: config.DefaultVectorDir(),
				OutputDir: outputDir,
				OllamaURL: defaultOllamaURL,
				ModelName: defaultModelName,
			})
			if err != nil {
				return err
			}
			if jsonOut {
				enc := json.NewEncoder(w)
				return enc.Encode(doctor.NewEnvelope("bundle", "healthy", map[string]any{
					"path":       result.Path,
					"file_count": result.FileCount,
				}))
			}
			fmt.Fprintln(w, result.Path)
			fmt.Fprintln(w, "attach this tarball to support / issue reports — contains probe outputs and recent audit log")
			return nil
		},
	}
	cmd.Flags().StringVar(&outputDir, "output-dir", "", "directory to write the tarball (default: system temp dir)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "output results as JSON")
	return cmd
}
