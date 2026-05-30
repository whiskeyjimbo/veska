package configcmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"

	"github.com/BurntSushi/toml"

	"github.com/whiskeyjimbo/veska/internal/infrastructure/embedding/elect"
	"github.com/whiskeyjimbo/veska/internal/platform/config"
)

// RunShow prints the effective resolved config: defaults merged with
// ~/.veska/config.toml and env-var overrides — the same pipeline the daemon
// uses at boot, so the operator sees the EXACT shape the daemon will observe
// (solov2-p6rt). Read-only.
func RunShow(w io.Writer, jsonOut bool) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("config show: %w", err)
	}
	liveEmbedder := elect.Marker(config.DefaultVectorDir())
	if jsonOut {
		// Sibling key `_live_embedder` carries the daemon's elected embedder so
		// callers don't read the [embedder] defaults (which only apply on
		// VESKA_EMBEDDER=ollama) as the truth. Empty string when no election
		// has run yet (solov2-awp6).
		envelope := struct {
			*config.Config
			LiveEmbedder string `json:"_live_embedder,omitempty"`
		}{&cfg, liveEmbedder}
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(envelope)
	}
	if liveEmbedder != "" {
		fmt.Fprintf(w, "# live embedder: %s\n", liveEmbedder)
		fmt.Fprintf(w, "# the [embedder] block below configures the Ollama branch and is\n")
		fmt.Fprintf(w, "# unused unless VESKA_EMBEDDER=ollama.\n\n")
	}
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(cfg); err != nil {
		return fmt.Errorf("config show: encode toml: %w", err)
	}
	_, werr := w.Write(buf.Bytes())
	return werr
}
