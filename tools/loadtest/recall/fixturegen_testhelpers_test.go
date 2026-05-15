//go:build eval

package recall

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/infrastructure/embedding/ollama"
)

// newOllamaStub returns an httptest.Server that responds to
// POST /api/embeddings with an Ollama-shaped JSON envelope whose first
// vector element varies per request (so callers can assert variation
// across nodes).
func newOllamaStub(t *testing.T, dim int) *httptest.Server {
	t.Helper()
	var n int64
	mux := http.NewServeMux()
	mux.HandleFunc("/api/embeddings", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method", http.StatusMethodNotAllowed)
			return
		}
		idx := atomic.AddInt64(&n, 1)
		vec := make([]float32, dim)
		vec[0] = float32(idx)
		_ = json.NewEncoder(w).Encode(map[string]any{"embedding": vec})
	})
	mux.HandleFunc("/api/tags", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"models":[]}`))
	})
	return httptest.NewServer(mux)
}

// newOllamaProvider wires the real ollama.Provider at a custom base URL —
// the same adapter the daemon uses in production.
func newOllamaProvider(baseURL string) *ollama.Provider {
	return ollama.New("nomic-embed-text", ollama.WithBaseURL(baseURL))
}
