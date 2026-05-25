package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/doctor"
)

// TestCheckEmbedderHealthDefaultIsInProcess verifies the default (no override)
// embedder health reports the elected in-process embedder and never claims to
// be probing Ollama — the bug behind solov2-rrm, where doctor reported
// "nomic-embed-text @ ollama" on the documented zero-dependency path.
func TestCheckEmbedderHealthDefaultIsInProcess(t *testing.T) {
	t.Setenv("VESKA_EMBEDDER", "")
	home := t.TempDir()
	h := checkEmbedderHealth(context.Background(), home)
	// solov2-yql1: a fresh home with no model2vec installed elects static-v2,
	// which is reported as 'degraded' so users see the fallback in `doctor
	// status` instead of only discovering it per-search. 'healthy' is also
	// acceptable here (e.g. when run from a fat build with model2vec embedded).
	if h.Status != "degraded" && h.Status != "healthy" {
		t.Fatalf("default embedder status = %q, want degraded or healthy", h.Status)
	}
	if !strings.Contains(h.Detail, "in-process") {
		t.Errorf("default embedder detail = %q, want it to mention in-process", h.Detail)
	}
	if strings.Contains(strings.ToLower(h.Detail), "ollama") {
		t.Errorf("default embedder detail = %q, must not mention ollama", h.Detail)
	}
	if h.Probe != nil {
		t.Errorf("default embedder should not run an Ollama probe, got %+v", h.Probe)
	}
}

// runDoctorEgress executes `veska doctor egress` and returns the combined
// output. A ProbeStatusError from a missing socket is expected in CI and not
// treated as a failure.
func runDoctorEgress(t *testing.T) string {
	t.Helper()
	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"doctor", "egress"})
	if err := root.Execute(); err != nil {
		var pse ProbeStatusError
		if !isProbeStatusError(err, &pse) {
			t.Fatalf("doctor egress: unexpected error: %v", err)
		}
	}
	return out.String()
}

// TestDoctorEgressOmitsVulnSourceWhenUnconfigured verifies the OSV endpoint
// does not appear when [vuln_source] is absent.
func TestDoctorEgressOmitsVulnSourceWhenUnconfigured(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VESKA_HOME", dir)
	t.Setenv("VESKA_CONFIG", filepath.Join(dir, "config.toml")) // missing file → defaults

	out := runDoctorEgress(t)
	if strings.Contains(out, "vuln_source") {
		t.Errorf("vuln_source endpoint listed when [vuln_source] unconfigured:\n%s", out)
	}
}

// TestDoctorEgressListsVulnSourceWhenConfigured verifies the OSV endpoint is
// listed when [vuln_source] provider="osv".
func TestDoctorEgressListsVulnSourceWhenConfigured(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte("[vuln_source]\nprovider = \"osv\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VESKA_HOME", dir)
	t.Setenv("VESKA_CONFIG", cfgPath)

	out := runDoctorEgress(t)
	if !strings.Contains(out, "vuln_source") {
		t.Errorf("vuln_source endpoint missing when [vuln_source] configured:\n%s", out)
	}
	if !strings.Contains(out, "osv-vulnerabilities.storage.googleapis.com") {
		t.Errorf("OSV dump URL missing from egress output:\n%s", out)
	}
}

// TestDoctorCmdName verifies doctorCmd returns a command named "doctor".
func TestDoctorCmdName(t *testing.T) {
	cmd := doctorCmd()
	if cmd.Use != "doctor" {
		t.Errorf("expected Use=doctor, got %q", cmd.Use)
	}
}

// TestDoctorSubcommands verifies all 8 required subcommands are registered.
func TestDoctorSubcommands(t *testing.T) {
	want := []string{
		"status",
		"egress",
		"storage",
		"embedder",
		"config",
		"post_promotion_queue",
		"pipelines",
		"bundle",
	}

	cmd := doctorCmd()
	found := make(map[string]bool)
	for _, sub := range cmd.Commands() {
		found[sub.Use] = true
	}

	for _, name := range want {
		if !found[name] {
			t.Errorf("missing subcommand %q", name)
		}
	}
}

// TestDoctorSubcommandsHaveJSONFlag verifies each subcommand has a --json flag.
func TestDoctorSubcommandsHaveJSONFlag(t *testing.T) {
	cmd := doctorCmd()
	for _, sub := range cmd.Commands() {
		f := sub.Flags().Lookup("json")
		if f == nil {
			t.Errorf("subcommand %q missing --json flag", sub.Use)
		}
	}
}

// TestDoctorStubSubcommandsExitZero verifies stub subcommands (those with no real probe yet)
// run without error and exit 0.
func TestDoctorStubSubcommandsExitZero(t *testing.T) {
	// These are pure stubs that always return ok.
	stubNames := []string{
		"pipelines",
		"bundle",
	}

	for _, name := range stubNames {
		t.Run(name, func(t *testing.T) {
			root := newRootCmd()
			var out bytes.Buffer
			root.SetOut(&out)
			root.SetErr(&out)
			root.SetArgs([]string{"doctor", name})
			if err := root.Execute(); err != nil {
				t.Errorf("subcommand %q: expected exit 0, got error: %v", name, err)
			}
		})
	}
}

// TestDoctorStorageExitZero verifies the storage subcommand runs without error
// (it only reads the filesystem and does not require live services).
func TestDoctorStorageExitZero(t *testing.T) {
	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"doctor", "storage"})
	if err := root.Execute(); err != nil {
		t.Errorf("subcommand storage: expected exit 0, got error: %v", err)
	}
}

// TestDoctorRealProbeSubcommandsRun verifies the live-probe subcommands (embedder, egress,
// config, status) execute without panicking.  In a test environment the probes may return
// degraded/broken but they must not crash or return an unexpected Go error.
func TestDoctorRealProbeSubcommandsRun(t *testing.T) {
	probeNames := []string{"embedder", "egress", "config", "status"}

	for _, name := range probeNames {
		t.Run(name, func(t *testing.T) {
			root := newRootCmd()
			var out bytes.Buffer
			root.SetOut(&out)
			root.SetErr(&out)
			root.SetArgs([]string{"doctor", name})
			// We do NOT assert err == nil — probes may legitimately return a
			// ProbeStatusError when services are unavailable in CI.  We only
			// assert the command ran (no panic) and any error is a ProbeStatusError.
			err := root.Execute()
			if err != nil {
				var pse ProbeStatusError
				if !isProbeStatusError(err, &pse) {
					t.Errorf("subcommand %q: unexpected non-probe error: %v", name, err)
				}
			}
		})
	}
}

// TestStatusRollupBacklogIsInformational verifies that a non-zero embedding
// backlog does NOT promote the rollup status (solov2-34rl). The backlog
// surfaces as its own line/field — the daemon as a whole remains healthy
// while the backfill drains. This is the contract that lets `doctor status`
// stop contradicting `eng_get_status`'s degraded_reasons:[embeddings_pending]:
// the two are now classifying the same backlog for different audiences,
// not disagreeing about whether the daemon is broken.
func TestStatusRollupBacklogIsInformational(t *testing.T) {
	t.Parallel()
	in := statusRollupInputs{
		EmbedderStatus:  "healthy",
		EgressStatus:    "healthy",
		ConfigStatus:    "healthy",
		IngestionStatus: "healthy",
		QueueStatus:     "healthy",
		EmbeddingBacklog: doctor.EmbeddingBacklogReport{
			Pending: 6480,
			Status:  "backfilling",
		},
	}
	got := computeStatusRollup(in)
	if got != "healthy" {
		t.Errorf("rollup with only backlog pending: want healthy, got %q", got)
	}
}

// TestStatusRollupBacklogDrainedIsHealthy is the symmetric guard: with
// pending=0, the rollup is unaffected (healthy when nothing else is wrong).
func TestStatusRollupBacklogDrainedIsHealthy(t *testing.T) {
	t.Parallel()
	in := statusRollupInputs{
		EmbedderStatus:  "healthy",
		EgressStatus:    "healthy",
		ConfigStatus:    "healthy",
		IngestionStatus: "healthy",
		QueueStatus:     "healthy",
		EmbeddingBacklog: doctor.EmbeddingBacklogReport{
			Pending: 0,
			Status:  "drained",
		},
	}
	if got := computeStatusRollup(in); got != "healthy" {
		t.Errorf("rollup with drained backlog: want healthy, got %q", got)
	}
}

// TestStatusRollupRealFaultStillBroken sanity-checks that adding the
// backlog signal didn't accidentally swallow a real fault elsewhere.
func TestStatusRollupRealFaultStillBroken(t *testing.T) {
	t.Parallel()
	in := statusRollupInputs{
		EmbedderStatus:   "healthy",
		EgressStatus:     "broken",
		ConfigStatus:     "healthy",
		IngestionStatus:  "healthy",
		QueueStatus:      "healthy",
		EmbeddingBacklog: doctor.EmbeddingBacklogReport{Pending: 6480, Status: "backfilling"},
	}
	if got := computeStatusRollup(in); got != "broken" {
		t.Errorf("rollup: want broken (egress=broken), got %q", got)
	}
}

// TestBacklogLabelBackfilling verifies the textual line format includes
// the pending count when the backlog is non-zero, so a junior reading
// `doctor status` sees the same number `eng_get_status` returns.
func TestBacklogLabelBackfilling(t *testing.T) {
	t.Parallel()
	got := backlogLabel(doctor.EmbeddingBacklogReport{Pending: 6480, Status: "backfilling"})
	want := "embedding_backlog=backfilling (6480 pending)"
	if got != want {
		t.Errorf("backlogLabel: want %q, got %q", want, got)
	}
}

// TestBacklogLabelDrained verifies the drained label has no parenthetical.
func TestBacklogLabelDrained(t *testing.T) {
	t.Parallel()
	got := backlogLabel(doctor.EmbeddingBacklogReport{Pending: 0, Status: "drained"})
	want := "embedding_backlog=drained"
	if got != want {
		t.Errorf("backlogLabel: want %q, got %q", want, got)
	}
}

// TestDoctorHelp verifies `veska doctor --help` lists subcommands.
func TestDoctorHelp(t *testing.T) {
	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"doctor", "--help"})
	if err := root.Execute(); err != nil {
		t.Fatalf("doctor --help failed: %v", err)
	}
	help := out.String()
	for _, sub := range []string{"status", "egress", "storage", "embedder", "config", "post_promotion_queue", "pipelines", "bundle"} {
		if !bytes.Contains([]byte(help), []byte(sub)) {
			t.Errorf("--help output missing subcommand %q", sub)
		}
	}
}
