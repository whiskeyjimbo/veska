// Package review holds the versioned LLM review prompt set and the per-kind
// response parsers used by the optional post-promotion review pipeline.
// Each review kind (security, contract_drift) ships as a cohesive unit: a
// versioned prompt template that renders the code-under-review into prompt
// text, plus a response parser that turns the model's text reply back into a
// slice of structured [ReviewFinding] values. A prompt without a defined
// output contract is not testable, so the two are never split apart.
// The package is application-layer: it may depend on internal/core/{domain,
// ports} but never on internal/infrastructure. The review goroutine (a
// separate task) is a thin orchestrator - it asks the [Loader] for a [Prompt],
// calls Render, hands the rendered text to a ports.LLMGenerator, and feeds the
// model output to Parse. The Prompt's Version is passed verbatim into
// ports.GenerateRequest.PromptTemplateVersion so cached outputs can be
// invalidated when a prompt template changes.
// Rendering is deterministic: a Prompt's Render is a pure function of its
// Input, so the goroutine's provenance InputHash is stable across runs.
package review
