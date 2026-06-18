// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

// Package embedderprobe provides shared health-check helpers for the Ollama
// embedding provider. It is used by both "veska init" and "veska doctor
// embedder" to avoid duplicating connectivity logic.
package embedderprobe

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"runtime"
	"strings"
	"time"

	"github.com/whiskeyjimbo/veska/internal/platform/health"
)

// ProbeResult holds the outcome of a full embedder probe.
type ProbeResult struct {
	// Reachable is true when the Ollama HTTP API responded to GET /api/tags.
	Reachable bool `json:"reachable"`
	// ModelPresent is true when the requested model appears in the tags list.
	ModelPresent bool `json:"model_present"`
	// EmbedOK is true when POST /api/embeddings returned a non-empty embedding vector.
	EmbedOK bool `json:"embed_ok"`
	// InstallHint is a platform-specific suggestion for installing Ollama and pulling
	// the model when the embedder is not healthy.
	InstallHint string `json:"install_hint,omitempty"`
	// Status is one of: "healthy", "degraded", "broken".
	//   healthy - all three checks passed.
	//   degraded - Ollama is reachable but model is missing or embed probe failed.
	//   broken - Ollama is not reachable.
	Status health.Status `json:"status"`
}

// ollamaTagsResponse is the minimal JSON shape returned by GET /api/tags.
type ollamaTagsResponse struct {
	Models []struct {
		Name string `json:"name"`
	} `json:"models"`
}

// ollamaEmbedRequest is the body sent to POST /api/embeddings.
type ollamaEmbedRequest struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
}

// ollamaEmbedResponse is the minimal JSON shape returned by POST /api/embeddings.
type ollamaEmbedResponse struct {
	Embedding []float64 `json:"embedding"`
}

// Probe runs three sequential checks against the Ollama instance at ollamaURL:
//  1. Reachable - GET /api/tags returns 200.
//  2. ModelPresent - modelName appears in the tags list.
//  3. ProbeEmbed - POST /api/embeddings with a dummy prompt returns a non-empty vector.
//
// Probe never returns a non-nil error; all failures are encoded in ProbeResult.
// A per-request timeout of 3 seconds is applied to each check.
func Probe(ctx context.Context, ollamaURL, modelName string) (*ProbeResult, error) {
	result := &ProbeResult{
		InstallHint: InstallHint(runtime.GOOS, modelName),
	}

	client := &http.Client{Timeout: 3 * time.Second}

	// ── Check 1: reachable ───────────────────────────────────────────────────
	tagsURL := fmt.Sprintf("%s/api/tags", ollamaURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, tagsURL, nil)
	if err != nil {
		result.Status = health.StatusBroken
		return result, nil
	}
	resp, err := client.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		if resp != nil {
			resp.Body.Close()
		}
		result.Status = health.StatusBroken
		return result, nil
	}
	result.Reachable = true

	var tags ollamaTagsResponse
	if err := json.NewDecoder(resp.Body).Decode(&tags); err != nil {
		resp.Body.Close()
		result.Status = health.StatusBroken
		return result, nil
	}
	resp.Body.Close()

	// ── Check 2: model present ───────────────────────────────────────────────
	// Ollama tags models like "nomic-embed-text:latest" but callers typically
	// pass the bare model name. Match either an exact equality (caller passed
	// "name:tag") or a prefix on "name:" (caller passed bare "name" - accept
	// any tag the user happens to have pulled). Ollama's /api/embeddings does
	// the same resolution server-side, so this mirrors what actually works.
	for _, m := range tags.Models {
		if m.Name == modelName || strings.HasPrefix(m.Name, modelName+":") {
			result.ModelPresent = true
			break
		}
	}
	if !result.ModelPresent {
		result.Status = health.StatusDegraded
		return result, nil
	}

	// ── Check 3: probe embed ─────────────────────────────────────────────────
	embedURL := fmt.Sprintf("%s/api/embeddings", ollamaURL)
	body, _ := json.Marshal(ollamaEmbedRequest{Model: modelName, Prompt: "test"})
	embedReq, err := http.NewRequestWithContext(ctx, http.MethodPost, embedURL, bytes.NewReader(body))
	if err != nil {
		result.Status = health.StatusDegraded
		return result, nil
	}
	embedReq.Header.Set("Content-Type", "application/json")

	embedResp, err := client.Do(embedReq)
	if err != nil {
		result.Status = health.StatusDegraded
		return result, nil
	}
	defer embedResp.Body.Close()

	var embedResult ollamaEmbedResponse
	if err := json.NewDecoder(embedResp.Body).Decode(&embedResult); err != nil {
		result.Status = health.StatusDegraded
		return result, nil
	}

	if len(embedResult.Embedding) == 0 {
		result.Status = health.StatusDegraded
		return result, nil
	}
	result.EmbedOK = true

	result.Status = health.StatusHealthy
	return result, nil
}

// InstallHint returns a platform-specific string describing how to install
// Ollama and pull the required model. goos should be runtime.GOOS.
// This function is pure - no network calls - so it is trivially testable.
func InstallHint(goos, modelName string) string {
	switch goos {
	case "darwin":
		return fmt.Sprintf("brew install ollama && ollama pull %s", modelName)
	default:
		// Linux and other platforms: prefer snap, fall back to curl pipe.
		return fmt.Sprintf(
			"snap install ollama && ollama pull %s\n"+
				"# or: curl -fsSL https://ollama.com/install.sh | sh && ollama pull %s",
			modelName, modelName,
		)
	}
}
