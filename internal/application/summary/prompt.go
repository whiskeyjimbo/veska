// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package summary

import (
	"encoding/json"
	"fmt"
	"strings"
)

// promptVersion is bumped whenever the prompt wording below changes, so a
// future summary cache can invalidate stale model outputs.
const promptVersion = "summary.v1"

// renderPrompt builds the summary prompt for one node. It is a deterministic
// pure function of its inputs.
func renderPrompt(repoID, branch, filePath string, n Node, body string) string {
	var b strings.Builder
	b.WriteString("You summarize a single code symbol in one sentence for a code-search index.\n")
	b.WriteString("Write a concise, factual description of what it does - no preamble, no restating its name.\n\n")
	fmt.Fprintf(&b, "Repository: %s\n", repoID)
	fmt.Fprintf(&b, "Branch: %s\n", branch)
	fmt.Fprintf(&b, "File: %s\n", filePath)
	fmt.Fprintf(&b, "Symbol: %s (%s)\n", n.Name, n.Kind)
	if sig := strings.TrimSpace(n.Signature); sig != "" {
		fmt.Fprintf(&b, "Signature: %s\n", sig)
	}
	b.WriteString("\n--- BEGIN CODE ---\n")
	b.WriteString(body)
	b.WriteString("\n--- END CODE ---\n\n")
	b.WriteString(`Respond with a single JSON object and nothing else: {"summary": "<one sentence, <=280 chars>"}.`)
	return b.String()
}

// parseSummary extracts the summary string from a model response. It tolerates
// a model that wraps the JSON in stray prose by scanning for the first JSON
// object; a fully unparseable response yields "" (the caller falls back to the
// heuristic).
func parseSummary(modelOutput string) string {
	raw := strings.TrimSpace(modelOutput)
	if start := strings.IndexByte(raw, '{'); start > 0 {
		raw = raw[start:]
	}
	if end := strings.LastIndexByte(raw, '}'); end >= 0 {
		raw = raw[:end+1]
	}
	var r summaryResponse
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		return ""
	}
	return strings.TrimSpace(r.Summary)
}
