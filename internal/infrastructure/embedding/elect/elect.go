// Package elect performs boot-time embedder election (solov2-1az).
//
// Embedders are a PICK-ONE ladder, never a stack: exactly one embedder
// owns the index at a time, because vectors from different models live
// in incompatible spaces and must never be mixed (see decision memory
// 'embedder-architecture'). This package replaces the composite
// Ollama→static fallback chain (solov2-soc), which mixed spaces.
//
// Election (default direction decided by the solov2-hd0 gate: model2vec
// is the default; Ollama is an opt-in max-quality override):
//
//	VESKA_EMBEDDER=ollama     → Ollama only
//	VESKA_EMBEDDER=model2vec  → model2vec only (error if not installed)
//	VESKA_EMBEDDER=static     → in-binary static-v2 only
//	unset (auto)              → model2vec if installed, else static-v2
//
// The elected embedder's ModelID is written to <VeskaHome>/embedder.locked
// — a sticky, descriptive marker. It equals the per-row model_id the
// embedder worker stamps, so the (deferred) background-reindex path can
// compare it against node_embedding_refs to detect a model switch.
//
// Transient outage of the elected embedder is NOT handled here: the
// provider returns ports.ErrEmbedderUnreachable at call time, the search
// service degrades to lexical/BM25 (already wired), and the embed worker
// pauses. Election is a boot-time decision, not a per-call one.
package elect

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/whiskeyjimbo/veska/internal/core/ports"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/embedding/model2vec"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/embedding/ollama"
	embedstatic "github.com/whiskeyjimbo/veska/internal/infrastructure/embedding/static"
)

// markerFile is the sticky election marker, relative to VeskaHome.
const markerFile = "embedder.locked"

// Override values for VESKA_EMBEDDER.
const (
	OverrideAuto      = ""
	OverrideOllama    = "ollama"
	OverrideModel2Vec = "model2vec"
	OverrideStatic    = "static"
)

// Config carries the inputs election needs. The composition root fills
// it from env + file config.
type Config struct {
	VeskaHome     string // data root holding the marker + static-model/
	Override      string // VESKA_EMBEDDER (OverrideAuto when unset)
	Model2VecName string // static-model dir name, e.g. "potion-code-16M"
	OllamaURL     string // Ollama base URL (for the ollama branch)
	EmbedModel    string // Ollama embedding model name
}

// Result is the outcome of an election.
type Result struct {
	// Provider is the elected EmbeddingProvider, ready to wire.
	Provider ports.EmbeddingProvider
	// Name is the elected provider's ModelID — also the marker contents.
	Name string
	// Previous is the marker value before this election ("" if none).
	Previous string
	// SwitchedModel is true when a prior marker existed and differs from
	// Name: the index was built with a different embedder, so a reindex
	// is required. The background reindex is a deferred follow-up; until
	// it lands, callers should surface this loudly.
	SwitchedModel bool
}

// Elect picks the embedder per the rules above, updates the sticky
// marker, and returns the chosen provider. A model2vec model dir name
// of "" defaults to "potion-code-16M".
func Elect(cfg Config) (Result, error) {
	modelName := cfg.Model2VecName
	if modelName == "" {
		modelName = "potion-code-16M"
	}

	provider, err := pick(cfg, modelName)
	if err != nil {
		return Result{}, err
	}

	name := provider.ModelID()
	prev, err := readMarker(cfg.VeskaHome)
	if err != nil {
		return Result{}, err
	}
	if err := writeMarker(cfg.VeskaHome, name); err != nil {
		return Result{}, err
	}
	return Result{
		Provider:      provider,
		Name:          name,
		Previous:      prev,
		SwitchedModel: prev != "" && prev != name,
	}, nil
}

// Resolve picks the embedder per the same rules as Elect but WITHOUT
// touching the sticky marker. Read-only consumers — notably the
// standalone `veska search` path that has no daemon — use it so they
// embed queries in the same vector space the daemon's index was built
// in, without claiming ownership of the marker (the daemon owns it).
func Resolve(cfg Config) (ports.EmbeddingProvider, error) {
	modelName := cfg.Model2VecName
	if modelName == "" {
		modelName = "potion-code-16M"
	}
	return pick(cfg, modelName)
}

// pick constructs the elected provider following the override / auto
// ladder. It does not probe network reachability — Ollama that is down
// still elects and degrades at call time via ErrEmbedderUnreachable.
func pick(cfg Config, modelName string) (ports.EmbeddingProvider, error) {
	switch strings.ToLower(strings.TrimSpace(cfg.Override)) {
	case OverrideOllama:
		return newOllama(cfg)
	case OverrideStatic:
		return embedstatic.New()
	case OverrideModel2Vec:
		p, err := model2vec.TryLoad(cfg.VeskaHome, modelName)
		if err != nil {
			if errors.Is(err, model2vec.ErrModelNotPresent) {
				return nil, fmt.Errorf("VESKA_EMBEDDER=model2vec but %q is not installed — run 'veska install model2vec': %w", modelName, err)
			}
			return nil, fmt.Errorf("elect: load model2vec: %w", err)
		}
		return p, nil
	case OverrideAuto:
		// Default direction: model2vec if installed, else static-v2.
		// Ollama is NOT auto-elected — it is opt-in via the override.
		p, err := model2vec.TryLoad(cfg.VeskaHome, modelName)
		if err == nil {
			return p, nil
		}
		if !errors.Is(err, model2vec.ErrModelNotPresent) {
			return nil, fmt.Errorf("elect: load model2vec: %w", err)
		}
		return embedstatic.New()
	default:
		return nil, fmt.Errorf("elect: unknown VESKA_EMBEDDER=%q (want ollama|model2vec|static or unset)", cfg.Override)
	}
}

// Ollama-branch defaults, applied only when Ollama is the elected
// embedder and the value was left unset — so they live with the one path
// that uses them, not as a daemon-wide implied default.
const (
	defaultOllamaEmbedModel = "nomic-embed-text"
	defaultOllamaURL        = "http://localhost:11434"
)

func newOllama(cfg Config) (ports.EmbeddingProvider, error) {
	model := cfg.EmbedModel
	if model == "" {
		model = defaultOllamaEmbedModel
	}
	baseURL := cfg.OllamaURL
	if baseURL == "" {
		baseURL = defaultOllamaURL
	}
	p, err := ollama.New(model, ollama.WithBaseURL(baseURL))
	if err != nil {
		return nil, fmt.Errorf("elect: ollama provider: %w", err)
	}
	return p, nil
}

// readMarker returns the current marker contents, or "" when absent.
func readMarker(veskaHome string) (string, error) {
	b, err := os.ReadFile(filepath.Join(veskaHome, markerFile))
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("elect: read marker: %w", err)
	}
	return strings.TrimSpace(string(b)), nil
}

// writeMarker persists name as the elected embedder marker.
func writeMarker(veskaHome, name string) error {
	if err := os.MkdirAll(veskaHome, 0o755); err != nil {
		return fmt.Errorf("elect: mkdir veska home: %w", err)
	}
	if err := os.WriteFile(filepath.Join(veskaHome, markerFile), []byte(name+"\n"), 0o644); err != nil {
		return fmt.Errorf("elect: write marker: %w", err)
	}
	return nil
}
