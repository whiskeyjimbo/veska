package doctorcmd

import (
	"context"
	"strings"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/platform/doctor"
)

// TestCheckEmbedderHealthDefaultIsInProcess verifies the default (no override)
// embedder health reports the elected in-process embedder and never claims to
// be probing Ollama - the bug behind, where doctor reported
// "nomic-embed-text @ ollama" on the documented zero-dependency path.
func TestCheckEmbedderHealthDefaultIsInProcess(t *testing.T) {
	t.Setenv("VESKA_EMBEDDER", "")
	home := t.TempDir()
	h := CheckEmbedderHealth(context.Background(), home)
	// a fresh home with no model2vec installed elects static-v2,
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

// TestStatusRollupBacklogIsInformational verifies that a non-zero embedding
// backlog does NOT promote the rollup status. The backlog
// surfaces as its own line/field - the daemon as a whole remains healthy
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
