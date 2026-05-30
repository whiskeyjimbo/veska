package composition

import (
	"fmt"
	"os"

	"github.com/whiskeyjimbo/veska/internal/application/search"
	"github.com/whiskeyjimbo/veska/internal/config"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/embedding/elect"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/vector"
)

// NewCLISearchService builds the search service for the one-shot `veska search`
// CLI path: it resolves the embedder from env/defaults, opens the local
// sqlite-vec store, and assembles the search.Service. The daemon builds its own
// search service from the elected provider and its configured vector backend
// (with metrics), so this is the CLI-side construction only — moved out of the
// Cobra file so cmd/veska/search.go is a thin adapter (solov2-u4mv.4).
func NewCLISearchService(pools *sqlite.Pools) (*search.Service, error) {
	prov, err := NewCLIEmbeddingProvider()
	if err != nil {
		return nil, err
	}
	vec, err := vector.NewVectorStorage("sqlite-vec", config.DefaultVectorDir())
	if err != nil {
		return nil, fmt.Errorf("search: open vector storage: %w", err)
	}
	nodes := sqlite.NewNodeLookupRepo(pools.ReadDB)
	return search.NewService(prov, vec, nodes), nil
}

// NewCLIEmbeddingProvider resolves the same embedder the daemon elects, reading
// VESKA_OLLAMA_URL / VESKA_EMBED_MODEL / VESKA_EMBEDDER with the historical CLI
// defaults. Shared by the CLI search service and the embedder-queue drain.
func NewCLIEmbeddingProvider() (ports.EmbeddingProvider, error) {
	baseURL := os.Getenv("VESKA_OLLAMA_URL")
	if baseURL == "" {
		baseURL = "http://localhost:11434"
	}
	model := os.Getenv("VESKA_EMBED_MODEL")
	if model == "" {
		model = "nomic-embed-text"
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
