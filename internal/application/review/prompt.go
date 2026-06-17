// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package review

import (
	"embed"
	"encoding/json"
	"fmt"
	"strings"
	"text/template"
)

//go:embed prompts/*.tmpl
var promptFS embed.FS

// templatedPrompt is the shared Prompt implementation: a parsed text/template
// plus a kind and version. Both review kinds use it; only the embedded
// template file and the version string differ.
type templatedPrompt struct {
	kind    ReviewKind
	version string
	tmpl    *template.Template
}

// newTemplatedPrompt parses the embedded template file for kind. A parse
// failure here is a build-time defect in a committed.tmpl file, so it
// surfaces as an error the loader turns loud at construction.
func newTemplatedPrompt(kind ReviewKind, version, file string) (*templatedPrompt, error) {
	raw, err := promptFS.ReadFile("prompts/" + file)
	if err != nil {
		return nil, fmt.Errorf("review: reading prompt %q: %w", file, err)
	}
	t, err := template.New(file).Option("missingkey=error").Parse(string(raw))
	if err != nil {
		return nil, fmt.Errorf("review: parsing prompt %q: %w", file, err)
	}
	return &templatedPrompt{kind: kind, version: version, tmpl: t}, nil
}

// Kind reports the review kind this prompt serves.
func (p *templatedPrompt) Kind() ReviewKind { return p.kind }

// Version reports the prompt-template version string.
func (p *templatedPrompt) Version() string { return p.version }

// Render executes the template against in. Rendering is a deterministic pure
// function of in: the template iterates only over scalar Input fields, so the
// same Input always yields byte-identical text.
func (p *templatedPrompt) Render(in Input) (string, error) {
	if strings.TrimSpace(in.Code) == "" {
		return "", ErrEmptyInput
	}
	var b strings.Builder
	if err := p.tmpl.Execute(&b, in); err != nil {
		return "", fmt.Errorf("review: rendering %s prompt: %w", p.kind, err)
	}
	return b.String(), nil
}

// Format reports the JSON Schema the prompt constrains the model output to.
// Every review kind shares the findings schema.
func (p *templatedPrompt) Format() json.RawMessage { return findingsSchema }

// Parse interprets a model's JSON response into structured findings using the
// package's shared json.Unmarshal-based parser.
func (p *templatedPrompt) Parse(modelOutput string) ([]ReviewFinding, error) {
	return parseJSON(p.kind, modelOutput)
}

// Compile-time check: *templatedPrompt satisfies Prompt.
var _ Prompt = (*templatedPrompt)(nil)
