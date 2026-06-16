package doctorcmd

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/whiskeyjimbo/veska/internal/infrastructure/embedding/elect"
	"github.com/whiskeyjimbo/veska/internal/platform/embedderprobe"
	"github.com/whiskeyjimbo/veska/internal/platform/health"
)

// EmbedderHealth describes the elected embedder's health for doctor output.
// The elected embedder — not Ollama unconditionally — is what doctor must
// report. For the in-process embedders (model2vec / static-v2) the provider
// is constructed locally and is healthy whenever election succeeds; no network
// probe applies. Only VESKA_EMBEDDER=ollama probes a remote Ollama instance.
type EmbedderHealth struct {
	Status health.Status              // healthy | degraded | broken
	Detail string                     // human-readable one-liner
	Probe  *embedderprobe.ProbeResult // non-nil only on the ollama path
}

// envOrDefault returns the environment value for key, or def when unset/empty.
func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// CheckEmbedderHealth resolves the elected embedder the same way the daemon
// and `veska init` do, and reports its health. It mirrors init's
// resolveInitEmbedder so the two never disagree about which embedder is live.
func CheckEmbedderHealth(ctx context.Context, home string) EmbedderHealth {
	override := os.Getenv("VESKA_EMBEDDER")
	if strings.EqualFold(override, elect.OverrideOllama) {
		return checkOllamaEmbedderHealth(ctx)
	}
	prov, err := elect.Resolve(elect.Config{VeskaHome: home, Override: override})
	if err != nil {
		return EmbedderHealth{Status: health.StatusBroken, Detail: fmt.Sprintf("election failed: %v", err)}
	}
	// Surface the static-v2 fallback as 'degraded' rather than 'healthy'
	// It is functional, but every eng_search_semantic call
	// returns 'low_quality_static_embedder' in degraded_reasons until the
	// user installs model2vec — that is not a healthy steady state.
	if prov.ModelID() == "veska-static-v2" {
		return EmbedderHealth{
			Status: health.StatusDegraded,
			Detail: prov.ModelID() + ", in-process (low-quality fallback — run `veska install model2vec` for higher-quality code search)",
		}
	}
	return EmbedderHealth{Status: health.StatusHealthy, Detail: prov.ModelID() + ", in-process"}
}

// checkOllamaEmbedderHealth probes a remote Ollama instance on the
// VESKA_EMBEDDER=ollama path.
func checkOllamaEmbedderHealth(ctx context.Context) EmbedderHealth {
	url := envOrDefault("VESKA_OLLAMA_URL", DefaultOllamaURL)
	model := envOrDefault("VESKA_EMBED_MODEL", DefaultModelName)
	res, err := embedderprobe.Probe(ctx, url, model)
	if err != nil {
		return EmbedderHealth{Status: health.StatusBroken, Detail: fmt.Sprintf("ollama probe error: %v", err)}
	}
	return EmbedderHealth{
		Status: res.Status,
		Detail: fmt.Sprintf("ollama %s @ %s (reachable=%v, model_present=%v, embed_ok=%v)",
			model, url, res.Reachable, res.ModelPresent, res.EmbedOK),
		Probe: res,
	}
}
