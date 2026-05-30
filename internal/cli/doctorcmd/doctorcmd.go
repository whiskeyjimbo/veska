// Package doctorcmd holds the delivery-layer logic behind the `veska doctor`
// command tree: the per-subsystem health-probe orchestration, the status
// rollup assembly, and the textual/JSON rendering of each probe report.
//
// The health-check DOMAIN logic (CheckEgress, CheckStorage, the
// EmbeddingBacklogReport, the envelope shape, …) already lives in
// internal/platform/doctor; doctorcmd orchestrates those probes, decides the
// per-subsystem and rollup status labels, and renders the operator-facing
// output. cmd/veska/doctor.go is reduced to Cobra command construction whose
// RunE bodies are thin calls into the Run* helpers here (solov2-0omh.6,
// following the cmd = glue / logic-in-packages pattern established by
// solov2-0omh.4 repocmd and solov2-0omh.5 searchcmd).
//
// Cross-cmd seams the cmd package owns — ProbeStatusError (its exit-code
// translation is shared with main.go and backup.go) and the in-process
// embedder default constants (shared with init.go) — are exported here and
// re-exported from cmd/veska so the other subcommands keep compiling against
// stable names.
package doctorcmd

const (
	// DefaultOllamaURL is the Ollama endpoint probed only on the
	// VESKA_EMBEDDER=ollama path.
	DefaultOllamaURL = "http://localhost:11434"
	// DefaultModelName is the default Ollama embedding model name.
	DefaultModelName = "nomic-embed-text"
)

// ProbeStatusError is returned by doctor subcommands when a probe yields a
// non-healthy status. main() translates it to the appropriate OS exit code.
type ProbeStatusError struct {
	Subsystem string
	Status    string // "degraded" or "broken"
}

func (e ProbeStatusError) Error() string {
	return e.Subsystem + ": " + e.Status
}

// IsProbeStatusError reports whether err is a ProbeStatusError and,
// if so, sets *out to its value.
func IsProbeStatusError(err error, out *ProbeStatusError) bool {
	if err == nil {
		return false
	}
	p, ok := err.(ProbeStatusError)
	if ok {
		*out = p
	}
	return ok
}

// ExitCodeForProbeStatus returns the conventional exit code for a probe status.
//
//	healthy  → 0
//	degraded → 0 (informational; the human-readable line is still printed to stderr)
//	broken   → 2
//
// Treating "degraded" as a non-failure keeps CI pipelines green when veska is
// merely in a transient warning state (e.g. a single unindexed repo, embedder
// warming up). Callers that want strict gating can re-introduce failure
// downstream by grepping the textual output or by parsing `--json` envelopes.
func ExitCodeForProbeStatus(status string) int {
	if status == "broken" {
		return 2
	}
	// "degraded" and "stopped" (solov2-bwly) are both informational —
	// non-zero status label, exit 0.
	return 0
}
