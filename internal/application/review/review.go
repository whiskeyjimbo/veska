package review

import (
	"encoding/json"
	"errors"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

// ReviewKind is a closed enum of the review kinds the prompt set covers.
type ReviewKind string

const (
	// KindSecurity reviews the code-under-review for security defects.
	KindSecurity ReviewKind = "security"
	// KindContractDrift reviews the code-under-review for behavioural or
	// API-contract drift relative to its prior signature.
	KindContractDrift ReviewKind = "contract_drift"
)

// Sentinel errors returned by the package. Callers match with errors.Is.
var (
	// ErrUnknownKind is returned by Loader.LoadPrompt for a ReviewKind that
	// has no registered prompt.
	ErrUnknownKind = errors.New("review: unknown review kind")
	// ErrEmptyInput is returned by Render when the Input carries no code to
	// review - a prompt with no subject is not worth a model call.
	ErrEmptyInput = errors.New("review: input has no code to review")
	// ErrMalformedResponse is returned by Parse when the model output cannot
	// be interpreted as the expected response contract.
	ErrMalformedResponse = errors.New("review: malformed model response")
)

// Input is the code-under-review handed to a Prompt's Render. It is the only
// thing Render depends on, so rendering is a pure, deterministic function of
// Input.
type Input struct {
	// RepoID and Branch identify the promoted state under review. They are
	// echoed into the rendered prompt for the model's context.
	RepoID string
	Branch string
	// FilePath is the path of the file under review.
	FilePath string
	// Code is the source text under review. Render rejects an empty Code.
	Code string
	// PriorSignature is the symbol's previously recorded signature. It is
	// only meaningful for KindContractDrift; the security prompt ignores it.
	PriorSignature string
}

// ReviewFinding is one structured issue parsed out of a model response. It
// carries enough to later be converted into a domain.Finding (a separate
// task owns that conversion); this package never builds a domain.Finding.
type ReviewFinding struct {
	// Title is a short one-line summary of the issue.
	Title string
	// Message is the full human-readable description of the issue.
	Message string
	// Severity is the model-assessed severity, validated against the
	// domain severity enum at parse time.
	Severity domain.Severity
	// Kind is the review kind that produced this finding.
	Kind ReviewKind
}

// Prompt is a versioned review prompt bundled with its response parser. The
// two travel together because a prompt is only testable against a defined
// output contract.
type Prompt interface {
	// Kind reports which review kind this prompt serves.
	Kind() ReviewKind
	// Version is the prompt-template version string. It is stable for a
	// given template and is passed verbatim into
	// ports.GenerateRequest.PromptTemplateVersion.
	Version() string
	// Render produces the prompt text for the given code-under-review. It
	// is a deterministic pure function of in; an Input with empty Code
	// returns ErrEmptyInput.
	Render(in Input) (string, error)
	// Format reports the structured-output schema the prompt expects the
	// model to conform to. The handler passes it verbatim into
	// ports.GenerateRequest.Format so the generator constrains the model to
	// schema-valid JSON.
	Format() json.RawMessage
	// Parse interprets a model's JSON response into structured findings.
	// A response the parser cannot interpret returns ErrMalformedResponse;
	// Parse never panics on malformed input.
	Parse(modelOutput string) ([]ReviewFinding, error)
}

// findingsSchema is the JSON Schema the review prompts constrain the model to.
// The model must return a JSON object with a "findings" array; an empty array
// means "no findings". It is passed as Ollama's structured-output 'format'
// parameter so the model output is validated against it.
var findingsSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "findings": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "severity": {"type": "string", "enum": ["info", "low", "medium", "high", "critical"]},
          "title": {"type": "string"},
          "message": {"type": "string"}
        },
        "required": ["severity", "title", "message"]
      }
    }
  },
  "required": ["findings"]
}`)
