package doctorcmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/whiskeyjimbo/veska/internal/application/extindex"
	"github.com/whiskeyjimbo/veska/internal/cli/repocmd"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/repo"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
	"github.com/whiskeyjimbo/veska/internal/platform/config"
	"github.com/whiskeyjimbo/veska/internal/platform/doctor"
	"github.com/whiskeyjimbo/veska/internal/platform/health"
)

// statusRollupInputs is the pure-data input to computeStatusRollup. It carries
// every per-subsystem signal the rollup considers, including the embedding
// backlog snapshot (solov2-34rl) which is surfaced but NOT permitted to
// promote the rollup status — see computeStatusRollup for the contract.
type statusRollupInputs struct {
	EmbedderStatus   string
	EgressStatus     string
	ConfigStatus     string
	IngestionStatus  string
	IngestionDetail  string
	QueueStatus      string
	QueueDetail      string
	DaemonNotRunning bool
	EmbeddingBacklog doctor.EmbeddingBacklogReport
}

// statusRollupJSONData is the JSON payload shape inside the `data` field of
// the `doctor status --json` envelope.
type statusRollupJSONData struct {
	Embedder         string `json:"embedder"`
	Egress           string `json:"egress"`
	Config           string `json:"config"`
	Ingestion        string `json:"ingestion"`
	IngestionDetail  string `json:"ingestion_detail,omitempty"`
	Queue            string `json:"queue"`
	QueueDetail      string `json:"queue_detail,omitempty"`
	EmbeddingBacklog string `json:"embedding_backlog"`
	PendingEmbeds    int    `json:"pending_embeds"`
}

// computeStatusRollup decides the rollup status from the per-subsystem signals.
//
// Rollup precedence (highest wins): broken > degraded > stopped > healthy.
//
// solov2-34rl: the embedding_backlog signal is INTENTIONALLY OMITTED from
// rollup classification. A non-zero backlog drives `eng_get_status`'s
// `degraded_reasons:[embeddings_pending]` because agents need that signal to
// pick between semantic and lexical search paths — but the daemon (embedder
// worker, queue, ingestion) is still healthy, work just isn't finished. A
// junior running `veska doctor` wants a go/no-go on the daemon, not a
// warmup-aware classification. The backlog is reported in the formatted
// output and the JSON payload as a separate field so both surfaces agree on
// the count (matching the README contract for `eng_search_semantic`).
func computeStatusRollup(in statusRollupInputs) string {
	statuses := []string{
		in.EmbedderStatus, in.EgressStatus, in.ConfigStatus,
		in.IngestionStatus, in.QueueStatus,
	}
	rollup := "healthy"
	for _, s := range statuses {
		switch s {
		case "broken":
			rollup = "broken"
		case "degraded":
			if rollup != "broken" {
				rollup = "degraded"
			}
		case "stopped":
			if rollup == "healthy" {
				rollup = "stopped"
			}
		}
	}
	return rollup
}

// backlogLabel renders the embedding backlog summary for the textual doctor
// output. Format examples:
//
//	embedding_backlog=drained
//	embedding_backlog=backfilling (6480 pending)
//	embedding_backlog=unknown
func backlogLabel(r doctor.EmbeddingBacklogReport) string {
	if r.Status == "backfilling" {
		return fmt.Sprintf("embedding_backlog=backfilling (%d pending)", r.Pending)
	}
	return "embedding_backlog=" + r.Status
}

// statusProbes holds the per-subsystem probe results gathered for the rollup.
type statusProbes struct {
	embedder         EmbedderHealth
	ingestionStatus  string
	ingestionDetail  string
	queueStatus      string
	queueDetail      string
	queueFailedRows  []doctor.FailedRow
	backlog          doctor.EmbeddingBacklogReport
	egressStatus     string
	daemonNotRunning bool
	configStatus     string
}

// gatherStatusProbes runs every probe the rollup depends on and derives the
// per-subsystem status labels (egress/config classification, daemon-stopped
// vs. crash-looped distinction).
func gatherStatusProbes(home string) statusProbes {
	p := statusProbes{}
	p.embedder = CheckEmbedderHealth(context.Background(), home)
	egressReport, _ := doctor.CheckEgress([]string{
		config.CLISockPath(),
		config.MCPSockPath(),
	})
	configReport, _ := doctor.CheckConfig(home)
	p.ingestionStatus, p.ingestionDetail = checkIngestion(context.Background())
	// solov2-j5ki: roll in post_promotion_queue health so the top-level
	// status doesn't report 'healthy' while a background pipeline
	// (auto_link, embed, revalidate, wiki) has failed rows or a deep
	// backlog. CheckPostPromotionQueue already classifies state.
	p.queueStatus = "healthy"
	if qr, qerr := doctor.CheckPostPromotionQueue(filepath.Join(home, "veska.db")); qerr == nil {
		p.queueStatus = string(qr.Status)
		p.queueFailedRows = qr.FailedRows
		if p.queueStatus != "healthy" {
			// solov2-gthm: include a pointer to the drilldown so a
			// junior who sees 'queue: N failed row(s)' in the rollup
			// has an obvious next command. The detail line is
			// printed as part of the status one-liner below.
			p.queueDetail = fmt.Sprintf("queue: %d failed row(s), %d state bucket(s); run `veska doctor post_promotion_queue` for details", len(qr.FailedRows), len(qr.Counts))
		}
	}

	// solov2-34rl: surface embedder backfill depth so doctor and
	// eng_get_status agree on the number. The backlog is informational
	// — it does NOT promote the rollup. See computeStatusRollup.
	p.backlog = probeEmbeddingBacklog(context.Background(), home)

	// Compute egress status: broken if any socket is missing. Track
	// whether BOTH sockets are missing — that is the unambiguous
	// "daemon never started" signal and warrants a friendlier message
	// than the generic "broken" rollup (solov2-eluk).
	p.egressStatus = "healthy"
	missing := 0
	for _, s := range egressReport.Sockets {
		if s.Status == "missing" {
			p.egressStatus = "broken"
			missing++
		}
	}
	p.daemonNotRunning = missing == len(egressReport.Sockets) && len(egressReport.Sockets) > 0

	// solov2-bwly: distinguish "the daemon has never been started"
	// (benign — operator just hasn't run `veska service start` yet)
	// from "the daemon crash-looped" (a real fault flagged by the
	// `<veskaHome>/broken` marker). The marker-less not-running
	// case should not be labelled "broken", which a fresh user sees
	// between `veska init` and `veska service start`.
	svcReport, _ := doctor.CheckService(home)
	daemonStopped := p.daemonNotRunning && !svcReport.BrokenMarkerPresent
	if daemonStopped {
		p.egressStatus = "stopped"
	}

	// Compute config status. solov2-lp44: the DB file is created
	// on first daemon boot, so a missing veska.db between `veska
	// init` and `veska service start` is the expected state —
	// surfacing it as "config=degraded" misled fresh users into
	// thinking config.toml was broken. Demote that single cause
	// to "pending" so it doesn't double-report what the
	// daemon-stopped line already says.
	p.configStatus = "healthy"
	if !configReport.DBExists {
		if daemonStopped {
			p.configStatus = "pending"
		} else {
			p.configStatus = "degraded"
		}
	}
	return p
}

// StatusOptions are the boolean flags for RunStatus. See QueueOptions for the
// single-flag-positional / multi-flag-struct convention (solov2-w8f9).
type StatusOptions struct {
	JSON    bool
	Verbose bool
}

// RunStatus performs the `doctor status` rollup across all subsystems, writing
// either the JSON envelope or the human-readable report to w. It returns a
// ProbeStatusError when the rollup is not healthy/stopped, mirroring the
// per-subsystem subcommands.
func RunStatus(w io.Writer, opts StatusOptions) error {
	jsonOut, verbose := opts.JSON, opts.Verbose
	home := config.DefaultVectorDir()
	p := gatherStatusProbes(home)

	inputs := statusRollupInputs{
		EmbedderStatus:   string(p.embedder.Status),
		EgressStatus:     p.egressStatus,
		ConfigStatus:     p.configStatus,
		IngestionStatus:  p.ingestionStatus,
		IngestionDetail:  p.ingestionDetail,
		QueueStatus:      p.queueStatus,
		QueueDetail:      p.queueDetail,
		DaemonNotRunning: p.daemonNotRunning,
		EmbeddingBacklog: p.backlog,
	}
	rollup := computeStatusRollup(inputs)

	if jsonOut {
		return json.NewEncoder(w).Encode(doctor.NewEnvelope("status", health.Status(rollup), statusRollupJSONData{
			Embedder:         inputs.EmbedderStatus,
			Egress:           inputs.EgressStatus,
			Config:           inputs.ConfigStatus,
			Ingestion:        inputs.IngestionStatus,
			IngestionDetail:  inputs.IngestionDetail,
			Queue:            inputs.QueueStatus,
			QueueDetail:      inputs.QueueDetail,
			EmbeddingBacklog: p.backlog.Status,
			PendingEmbeds:    p.backlog.Pending,
		}))
	}

	renderStatusText(w, p, rollup, inputs, verbose)
	if rollup != "healthy" && rollup != "stopped" {
		return ProbeStatusError{Subsystem: "status", Status: rollup}
	}
	if rollup == "stopped" {
		return ProbeStatusError{Subsystem: "status", Status: "stopped"}
	}
	return nil
}

// renderStatusText writes the human-readable status rollup, including the
// daemon-down framing and the optional --verbose failed-queue-row dump.
func renderStatusText(w io.Writer, p statusProbes, rollup string, inputs statusRollupInputs, verbose bool) {
	detail := ""
	if inputs.IngestionDetail != "" {
		detail = " — " + inputs.IngestionDetail
	}
	if inputs.QueueDetail != "" {
		if detail == "" {
			detail = " — "
		} else {
			detail += "; "
		}
		detail += inputs.QueueDetail
	}
	backlogStr := backlogLabel(p.backlog)
	// solov2-e141: when the daemon is down, lead with that fact and
	// flag the other subsystem labels as on-disk checks. Their
	// 'healthy' (embedder weights present, config readable, DB query
	// succeeded) was confusing readers into thinking the daemon was
	// fine. The rollup is already 'broken' in that case; this just
	// clarifies WHY the other labels say what they say.
	if p.daemonNotRunning {
		// Lead with the rollup, not a hard-coded "broken". When the
		// only non-healthy thing is "daemon not started yet", the
		// rollup is "stopped" and that's what the user should see.
		// When another subsystem is independently broken, the rollup
		// (and lead) is still "broken" — a real fault.
		fmt.Fprintf(w, "status: %s — daemon is not running (egress=%s)\n", rollup, p.egressStatus)
		fmt.Fprintf(w, "  on-disk checks (independent of daemon): embedder=%s, config=%s, ingestion=%s, queue=%s, %s%s\n",
			p.embedder.Status, p.configStatus, p.ingestionStatus, p.queueStatus, backlogStr, detail)
		fmt.Fprintln(w, "  hint: start it with `veska service start` (or `veska-daemon &` for a quick try)")
	} else {
		fmt.Fprintf(w, "status: %s (embedder=%s, egress=%s, config=%s, ingestion=%s, queue=%s, %s)%s\n",
			rollup, p.embedder.Status, p.egressStatus, p.configStatus, p.ingestionStatus, p.queueStatus, backlogStr, detail)
	}
	// "stopped" reports a benign operator state (daemon never
	// started, no broken marker) and uses the same exit semantics as
	// "degraded": non-zero rollup label, zero exit (solov2-bwly).
	// solov2-gthm: --verbose dumps the actual failed queue rows
	// inline so juniors who hit 'queue: N failed row(s)' do not
	// have to discover `doctor post_promotion_queue` separately.
	if verbose && len(p.queueFailedRows) > 0 {
		fmt.Fprintln(w, "  failed queue rows:")
		for _, f := range p.queueFailedRows {
			fmt.Fprintf(w, "    seq=%d repo=%s branch=%s kind=%s attempts=%d err=%s\n",
				f.Seq, f.RepoID, f.Branch, f.WorkKind, f.Attempts, f.Error)
		}
	}
}

// probeEmbeddingBacklog opens the local sqlite DB and runs the embedding
// backlog probe. Falls back to an "unknown" report if the DB cannot be
// opened (e.g. fresh `veska init` hasn't created it yet, or the daemon
// holds the lock) — never returns an error, since this signal is purely
// informational (solov2-34rl).
func probeEmbeddingBacklog(ctx context.Context, home string) doctor.EmbeddingBacklogReport {
	db, closeFn, err := repocmd.OpenLocalDB()
	if err != nil {
		return doctor.EmbeddingBacklogReport{Status: "unknown"}
	}
	defer closeFn()
	refs := sqlite.NewEmbeddingRefsRepo(db, db)
	rep, _ := doctor.CheckEmbeddingBacklog(ctx, refs)
	_ = home
	return rep
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
	db, closeFn, err := repocmd.OpenLocalDB()
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
	// Pull scan progress so unindexed repos that are actively scanning
	// surface as e.g. "9092cd5e0cff promoting/300" — tells the user the
	// degraded state is progressing vs. idle (solov2-u9h9 follow-up).
	progress := repocmd.FetchScanProgress(ctx)
	var unindexed []string
	for _, r := range recs {
		// Synthetic ext:<module> repos never get a LastPromotedSHA — they
		// have no git history. Skipping them avoids reporting "1 unindexed
		// repo(s): [ext:github.c]" on a healthy `deps index` workspace
		// (solov2-puga).
		if strings.HasPrefix(r.RepoID, extindex.SyntheticRepoIDPrefix) {
			continue
		}
		if r.LastPromotedSHA == "" {
			unindexed = append(unindexed, ingestionRepoLabel(r, progress))
		}
	}
	if len(unindexed) == 0 {
		return "healthy", ""
	}
	return "degraded", fmt.Sprintf("%d unindexed repo(s): %v", len(unindexed), unindexed)
}

// ingestionRepoLabel renders a single unindexed repo entry, appending its
// live scan phase/progress when a cold scan is in flight.
func ingestionRepoLabel(r repo.Record, progress map[string]repocmd.ScanProgressRow) string {
	short := r.RepoID
	if len(short) > 12 {
		short = short[:12]
	}
	if p, ok := progress[r.RepoID]; ok && p.Phase != "" {
		return fmt.Sprintf("%s %s/%d", short, p.Phase, p.FilesSeen)
	}
	return short
}
