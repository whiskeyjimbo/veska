package composition

import (
	"fmt"
	"os"

	"github.com/whiskeyjimbo/veska/internal/application/search"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/embedding/elect"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/embedding/ollama"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/vector"
	"github.com/whiskeyjimbo/veska/internal/platform/config"
)

// NewCLISearchService builds the search service for the one-shot CLI path. It
// resolves the embedder from environment variables or defaults, opens local
// storage, and assembles the search service. This construction is moved out
// of the CLI command package to keep command delivery files as thin adapters.
func NewCLISearchService(pools *sqlite.Pools) (*search.Service, error) {
	prov, err := NewCLIEmbeddingProvider()
	if err != nil {
		return nil, err
	}
	vec, err := vector.NewVectorStorage(vector.BackendMemory, config.DefaultVectorDir())
	if err != nil {
		return nil, fmt.Errorf("search: open vector storage: %w", err)
	}
	nodes := sqlite.NewNodeLookupRepo(pools.ReadDB)
	svc, err := search.NewService(prov, vec, nodes)
	if err != nil {
		return nil, fmt.Errorf("search: build service: %w", err)
	}
	return svc, nil
}

// NewCLIEmbeddingProvider resolves the same embedder that the daemon elects by
// reading environment overrides and historical CLI defaults. It is shared by
// both the CLI search service and the embedder-queue worker.
func NewCLIEmbeddingProvider() (ports.EmbeddingProvider, error) {
	baseURL := os.Getenv("VESKA_OLLAMA_URL")
	if baseURL == "" {
		baseURL = ollama.DefaultBaseURL
	}
	model := os.Getenv("VESKA_EMBED_MODEL")
	if model == "" {
		model = ollama.DefaultModel
	}
	prov, err := elect.Resolve(elect.Config{
		VeskaHome:     config.DefaultVectorDir(),
		Override:      os.Getenv("VESKA_EMBEDDER"),
		Model2VecName: "potion-code-16M",
		OllamaURL:     baseURL,
		EmbedModel:    model,
	})
	if err != nil {
		return nil, fmt.Errorf("search: resolve embedder: %w", err)
	}
	return prov, nil
}
