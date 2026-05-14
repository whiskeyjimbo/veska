package embedderprobe_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/embedderprobe"
)

// makeServer creates a test HTTP server that handles /api/tags and /api/embeddings.
// modelPresent controls whether the model appears in the tags response.
// embedResp is the raw JSON returned from /api/embeddings.
func makeServer(t *testing.T, modelName string, modelPresent bool, embedResp string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			w.Header().Set("Content-Type", "application/json")
			type model struct {
				Name string `json:"name"`
			}
			type tagsResp struct {
				Models []model `json:"models"`
			}
			resp := tagsResp{}
			if modelPresent {
				resp.Models = []model{{Name: modelName}}
			}
			_ = json.NewEncoder(w).Encode(resp)
		case "/api/embeddings":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(embedResp))
		default:
			http.NotFound(w, r)
		}
	}))
}

func TestProbeHealthy(t *testing.T) {
	srv := makeServer(t, "nomic-embed-text", true, `{"embedding":[0.1,0.2]}`)
	defer srv.Close()

	result, err := embedderprobe.Probe(context.Background(), srv.URL, "nomic-embed-text")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Reachable {
		t.Error("want Reachable=true")
	}
	if !result.ModelPresent {
		t.Error("want ModelPresent=true")
	}
	if !result.EmbedOK {
		t.Error("want EmbedOK=true")
	}
	if result.Status != "healthy" {
		t.Errorf("want Status=healthy, got %q", result.Status)
	}
}

func TestProbeDegraded_ModelMissing(t *testing.T) {
	srv := makeServer(t, "nomic-embed-text", false, `{"embedding":[0.1]}`)
	defer srv.Close()

	result, err := embedderprobe.Probe(context.Background(), srv.URL, "nomic-embed-text")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Reachable {
		t.Error("want Reachable=true")
	}
	if result.ModelPresent {
		t.Error("want ModelPresent=false")
	}
	if result.Status != "degraded" {
		t.Errorf("want Status=degraded, got %q", result.Status)
	}
}

func TestProbeDegraded_EmbedFail(t *testing.T) {
	srv := makeServer(t, "nomic-embed-text", true, `{"embedding":[]}`)
	defer srv.Close()

	result, err := embedderprobe.Probe(context.Background(), srv.URL, "nomic-embed-text")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Reachable {
		t.Error("want Reachable=true")
	}
	if !result.ModelPresent {
		t.Error("want ModelPresent=true")
	}
	if result.EmbedOK {
		t.Error("want EmbedOK=false")
	}
	if result.Status != "degraded" {
		t.Errorf("want Status=degraded, got %q", result.Status)
	}
}

func TestProbeBroken(t *testing.T) {
	// Use a URL that refuses connection immediately.
	result, err := embedderprobe.Probe(context.Background(), "http://127.0.0.1:1", "nomic-embed-text")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Reachable {
		t.Error("want Reachable=false")
	}
	if result.Status != "broken" {
		t.Errorf("want Status=broken, got %q", result.Status)
	}
}

func TestInstallHintDarwin(t *testing.T) {
	hint := embedderprobe.InstallHint("darwin", "nomic-embed-text")
	if !strings.Contains(hint, "brew") {
		t.Errorf("expected hint to contain 'brew', got: %q", hint)
	}
}

func TestInstallHintLinux(t *testing.T) {
	hint := embedderprobe.InstallHint("linux", "nomic-embed-text")
	if !strings.Contains(hint, "ollama") {
		t.Errorf("expected hint to contain 'ollama', got: %q", hint)
	}
}
