// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

// Package elect performs boot-time embedder election to pick exactly one embedder
// to own the index. Vectors from different models reside in incompatible spaces and must not be mixed.
//
// The elected embedder's ID is written to a sticky lock file (`embedder.locked`) inside the Veska home directory.
// Election is a static decision made at startup rather than dynamically evaluated per-call.
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

// markerFile defines the name of the file indicating the locked embedder.
const markerFile = "embedder.locked"

// Override constants specify values for the VESKA_EMBEDDER environment variable.
const (
	OverrideAuto      = ""
	OverrideOllama    = "ollama"
	OverrideModel2Vec = "model2vec"
	OverrideStatic    = "static"
)

// Config carries the parameters needed to elect or resolve the active embedder.
type Config struct {
	VeskaHome     string // data root holding the marker + static-model/
	Override      string // VESKA_EMBEDDER (OverrideAuto when unset)
	Model2VecName string // static-model dir name, e.g. "potion-code-16M"
	OllamaURL     string // Ollama base URL (for the ollama branch)
	EmbedModel    string // Ollama embedding model name
}

// Result encapsulates the outcome of an embedder election.
type Result struct {
	// Provider is the elected EmbeddingProvider, ready to wire.
	Provider ports.EmbeddingProvider
	// Name is the elected provider's ModelID - also the marker contents.
	Name string
	// Previous is the marker value before this election ("" if none).
	Previous string
	// SwitchedModel indicates whether the newly elected embedder differs from the previously locked model.
	SwitchedModel bool
	// Ollama is true when the elected embedder is the Ollama network branch.
	// Local embedders (model2vec/static) are fast and must not be rate-limited
	// like the network path.
	Ollama bool
}

// Elect selects the appropriate embedder, writes the selection to the lock file, and returns the provider.
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
		Ollama:        cfg.Override == OverrideOllama,
	}, nil
}

// Resolve determines the elected embedder following the standard rules without writing to the lock file.
func Resolve(cfg Config) (ports.EmbeddingProvider, error) {
	modelName := cfg.Model2VecName
	if modelName == "" {
		modelName = "potion-code-16M"
	}
	return pick(cfg, modelName)
}

// pick creates the selected embedding provider based on config override priorities.
func pick(cfg Config, modelName string) (ports.EmbeddingProvider, error) {
	switch strings.ToLower(strings.TrimSpace(cfg.Override)) {
	case OverrideOllama:
		return newOllama(cfg)
	case OverrideStatic:
		return embedstatic.New()
	case OverrideModel2Vec:
		p, err := tryModel2Vec(cfg, modelName)
		if err != nil {
			return nil, err
		}
		if p == nil {
			return nil, fmt.Errorf("VESKA_EMBEDDER=model2vec but %q is not installed and this binary has no embedded model - run 'veska install model2vec'", modelName)
		}
		return p, nil
	case OverrideAuto:
		// Default to model2vec if available, falling back to static embeddings.
		p, err := tryModel2Vec(cfg, modelName)
		if err != nil {
			return nil, err
		}
		if p != nil {
			return p, nil
		}
		return embedstatic.New()
	default:
		return nil, fmt.Errorf("elect: unknown VESKA_EMBEDDER=%q (want ollama|model2vec|static or unset)", cfg.Override)
	}
}

// Default settings for Ollama configuration.
const (
	defaultOllamaEmbedModel = "nomic-embed-text"
	defaultOllamaURL        = "http://localhost:11434"
)

// tryModel2Vec attempts to load the Model2Vec provider from disk or falls back to binary-embedded models.
func tryModel2Vec(cfg Config, modelName string) (ports.EmbeddingProvider, error) {
	p, err := model2vec.TryLoad(cfg.VeskaHome, modelName)
	if err == nil {
		return p, nil
	}
	if !errors.Is(err, model2vec.ErrModelNotPresent) {
		return nil, fmt.Errorf("elect: load model2vec: %w", err)
	}
	if embedded, ok := model2vec.Embedded(); ok {
		return embedded, nil
	}
	return nil, nil
}

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

// Marker returns the elected embedder's ModelID recorded in the sticky
// marker, or "" when no election has happened yet. Read-only consumers (e.g.
// the daemon's config tool) use it to report which embedder owns the index.
func Marker(veskaHome string) string {
	name, _ := readMarker(veskaHome)
	return name
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
