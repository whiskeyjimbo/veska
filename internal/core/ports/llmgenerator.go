package ports

import (
	"context"
	"encoding/json"
)

// GenerateRequest carries the inputs for a single text-generation call.
type GenerateRequest struct {
	// Prompt is the text prompt sent to the model.
	Prompt string

	// MaxTokens is the upper bound on tokens the model may generate.
	// A value of zero means the implementation should use its default limit.
	MaxTokens int

	// PromptTemplateVersion identifies the version of the prompt template the
	// caller used to build Prompt. The generator does not interpret it; it is
	// echoed verbatim into GenerateResponse.Provenance so a stored review can
	// be traced back to the exact template that produced it.
	PromptTemplateVersion string

	// Format optionally requests structured output. When non-empty it is a
	// JSON Schema (a json.RawMessage) the model output must conform to;
	// generators that support structured output (e.g. Ollama's /api/generate
	// 'format' parameter) constrain the model to schema-valid JSON. A zero
	// value means plain-text output — the default — so existing callers and
	// generators are unaffected.
	Format json.RawMessage
}

// Provenance records how a GenerateResponse was produced so a downstream
// consumer (the review pipeline) can audit and reproduce a result.
type Provenance struct {
	// ModelID is the model identifier the generator used.
	ModelID string

	// PromptTemplateVersion is echoed from the originating GenerateRequest.
	PromptTemplateVersion string

	// InputHash is a stable hash (sha256, hex-encoded) of the request prompt.
	InputHash string
}

// TokenUsage records the actual token counts a generation call consumed. The
// counts are reported by the model itself — no estimator is involved. Total is
// derivable as PromptTokens + CompletionTokens.
type TokenUsage struct {
	// PromptTokens is the number of tokens in the prompt the model evaluated.
	PromptTokens int

	// CompletionTokens is the number of tokens the model generated.
	CompletionTokens int
}

// Total returns the combined prompt + completion token count.
func (u TokenUsage) Total() int { return u.PromptTokens + u.CompletionTokens }

// GenerateResponse carries the output of a single text-generation call.
type GenerateResponse struct {
	// Text is the generated text returned by the model.
	Text string

	// Provenance records the model, prompt-template version, and input hash
	// behind Text. Implementations must populate it on a successful call.
	Provenance Provenance

	// Usage carries the actual prompt and completion token counts the call
	// consumed. Implementations populate it on a successful call when the
	// underlying model reports usage; a zero value means "not reported".
	Usage TokenUsage
}

// LLMGenerator is the port for text generation against a large language model.
// Implementations are provided by infrastructure adapters (e.g. Ollama,
// OpenAI, Anthropic Claude).
type LLMGenerator interface {
	// Generate sends req to the underlying model and returns the generated
	// text together with its Provenance. Implementations must respect ctx
	// cancellation and deadline.
	Generate(ctx context.Context, req GenerateRequest) (GenerateResponse, error)
}
