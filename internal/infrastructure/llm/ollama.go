// Package llm provides LLMGenerator implementations for the veska module.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

const (
	// defaultOllamaBase is the default Ollama API base URL.
	defaultOllamaBase = "http://localhost:11434"

	// defaultOllamaModel is the default text-generation model used when none
	// is specified.
	defaultOllamaModel = "llama3"
)

// ollamaGenerateRequest is the JSON body for POST /api/generate.
type ollamaGenerateRequest struct {
	Model      string `json:"model"`
	Prompt     string `json:"prompt"`
	NumPredict int    `json:"num_predict,omitempty"`
	Stream     bool   `json:"stream"`
}

// ollamaGenerateResponse is the JSON body returned by POST /api/generate
// when stream is false.
type ollamaGenerateResponse struct {
	Response string `json:"response"`
}

// OllamaGenerator is an LLMGenerator backed by a locally running Ollama
// instance. It POSTs to /api/generate with stream:false and returns the
// complete generated text in a single call.
//
// OllamaGenerator is safe for concurrent use.
type OllamaGenerator struct {
	baseURL string
	model   string
	client  *http.Client
}

// Compile-time interface satisfaction check.
var _ ports.LLMGenerator = (*OllamaGenerator)(nil)

// NewOllamaGenerator constructs an OllamaGenerator with the given base URL and
// model. Pass empty strings to use the defaults (http://localhost:11434 and
// "llama3"). The provided http.Client is used for all requests; pass nil to use
// http.DefaultClient.
func NewOllamaGenerator(baseURL, model string, client *http.Client) *OllamaGenerator {
	if baseURL == "" {
		baseURL = defaultOllamaBase
	}
	if model == "" {
		model = defaultOllamaModel
	}
	if client == nil {
		client = http.DefaultClient
	}
	return &OllamaGenerator{
		baseURL: baseURL,
		model:   model,
		client:  client,
	}
}

// Generate sends req to the Ollama /api/generate endpoint and returns the
// generated text. It respects ctx cancellation.
func (g *OllamaGenerator) Generate(ctx context.Context, req ports.GenerateRequest) (ports.GenerateResponse, error) {
	body := ollamaGenerateRequest{
		Model:      g.model,
		Prompt:     req.Prompt,
		NumPredict: req.MaxTokens,
		Stream:     false,
	}

	encoded, err := json.Marshal(body)
	if err != nil {
		return ports.GenerateResponse{}, fmt.Errorf("ollama: marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, g.baseURL+"/api/generate", bytes.NewReader(encoded))
	if err != nil {
		return ports.GenerateResponse{}, fmt.Errorf("ollama: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := g.client.Do(httpReq)
	if err != nil {
		return ports.GenerateResponse{}, fmt.Errorf("ollama: POST /api/generate: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return ports.GenerateResponse{}, fmt.Errorf("ollama: POST /api/generate: status %d", resp.StatusCode)
	}

	var out ollamaGenerateResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return ports.GenerateResponse{}, fmt.Errorf("ollama: decode response: %w", err)
	}

	return ports.GenerateResponse{Text: out.Response}, nil
}
