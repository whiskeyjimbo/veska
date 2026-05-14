package ports

import "context"

// GenerateRequest carries the inputs for a single text-generation call.
type GenerateRequest struct {
	// Prompt is the text prompt sent to the model.
	Prompt string

	// MaxTokens is the upper bound on tokens the model may generate.
	// A value of zero means the implementation should use its default limit.
	MaxTokens int
}

// GenerateResponse carries the output of a single text-generation call.
type GenerateResponse struct {
	// Text is the generated text returned by the model.
	Text string
}

// LLMGenerator is the port for text generation against a large language model.
// Implementations are provided by infrastructure adapters (e.g. Ollama,
// OpenAI, Anthropic Claude).
type LLMGenerator interface {
	// Generate sends req to the underlying model and returns the generated
	// text. Implementations must respect ctx cancellation and deadline.
	Generate(ctx context.Context, req GenerateRequest) (GenerateResponse, error)
}
