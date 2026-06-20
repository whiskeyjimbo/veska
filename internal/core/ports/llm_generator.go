// SPDX-License-Identifier: AGPL-3.0-only

package ports

import (
	"context"
	"encoding/json"
)

// GenerateRequest carries the inputs for a single text-generation call.
type GenerateRequest struct {
	Prompt string

	// A value of zero means the implementation should use its default limit.
	MaxTokens int

	// Echoed verbatim into GenerateResponse.Provenance so a stored review can
	// be traced back to the exact template that produced it.
	PromptTemplateVersion string

	// Format optionally requests structured output. When non-empty it is a
	// JSON Schema the model output must conform to; generators that support
	// structured output constrain the model to schema-valid JSON. A zero value
	// means plain-text output.
	Format json.RawMessage
}

// Provenance records how a GenerateResponse was produced so a downstream
// consumer (the review pipeline) can audit and reproduce a result.
type Provenance struct {
	ModelID string

	// Echoed from the originating GenerateRequest.
	PromptTemplateVersion string

	// InputHash is a stable hex-encoded sha256 hash of the request prompt.
	InputHash string
}

// TokenUsage records token counts reported by the model itself; no estimator
// is involved.
type TokenUsage struct {
	PromptTokens     int
	CompletionTokens int
}

func (u TokenUsage) Total() int { return u.PromptTokens + u.CompletionTokens }

// GenerateResponse carries the output of a single text-generation call.
type GenerateResponse struct {
	Text string

	// Provenance records the model, prompt-template version, and input hash
	// behind the text. Implementations must populate it on a successful call.
	Provenance Provenance

	// Usage carries the actual prompt and completion token counts the call
	// consumed. Implementations populate it on a successful call when the
	// underlying model reports usage; a zero value means not reported.
	Usage TokenUsage
}

// LLMGenerator is the port for text generation against a large language model.
type LLMGenerator interface {
	// Generate sends req to the underlying model and returns the generated
	// text together with its Provenance. Implementations must respect context
	// cancellation and deadline.
	Generate(ctx context.Context, req GenerateRequest) (GenerateResponse, error)
}
