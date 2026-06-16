package doctor

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/whiskeyjimbo/veska/internal/platform/health"
)

// EmbedderReport holds the result of probing the Ollama embedding provider.
type EmbedderReport struct {
	OllamaURL string `json:"ollama_url"`
	ModelName string `json:"model_name"`
	// Status is one of: "healthy", "degraded", "broken".
	// healthy — Ollama reachable and model present.
	// degraded — Ollama reachable but model not in the list.
	// broken — Ollama unreachable.
	Status health.Status `json:"status"`
}

// ollamaTagsResponse is the minimal subset of the /api/tags JSON body we need.
type ollamaTagsResponse struct {
	Models []struct {
		Name string `json:"name"`
	} `json:"models"`
}

// CheckEmbedder probes the Ollama instance at ollamaURL and checks whether
// modelName is available. It uses a 3-second timeout and never returns a
// non-nil error — connectivity failures are reflected in the Status field.
func CheckEmbedder(ollamaURL, modelName string) (EmbedderReport, error) {
	report := EmbedderReport{
		OllamaURL: ollamaURL,
		ModelName: modelName,
	}

	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(fmt.Sprintf("%s/api/tags", ollamaURL))
	if err != nil {
		report.Status = health.StatusBroken
		return report, nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		report.Status = health.StatusBroken
		return report, nil
	}

	var tags ollamaTagsResponse
	if err := json.NewDecoder(resp.Body).Decode(&tags); err != nil {
		// Unexpected response body — treat as broken.
		report.Status = health.StatusBroken
		return report, nil
	}

	for _, m := range tags.Models {
		if m.Name == modelName {
			report.Status = health.StatusHealthy
			return report, nil
		}
	}

	// Ollama is up but the requested model is not present.
	report.Status = health.StatusDegraded
	return report, nil
}
