// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

// Package summary implements the optional LLM summary lane: an off-by-default
// post-promotion queue handler that attaches a short natural-language summary
// to each promoted node via the [llm_generator] slot. It mirrors the review
// pipeline (application/review) but writes back to nodes.short_summary rather
// than emitting findings. See docs/design/11-pipelines/summary-worker.md
// (FEATURE-SUMMARY-001).
package summary

import (
	"context"
	"encoding/json"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

// Node is the minimal projection the summary lane needs to summarize one
// promoted node. RawContent is not persisted on nodes, so the handler slices
// the node's body out of the on-disk file using LineStart/LineEnd.
type Node struct {
	NodeID    string
	Kind      string
	Name      string
	Signature string
	LineStart int
	LineEnd   int
}

// Store loads the summarizable nodes of a promoted file and persists their
// generated summaries. It is consumer-owned (ISP): sized to exactly what the
// handler needs, implemented by sqlite.SummaryStore.
type Store interface {
	// PromotedNodes returns the summarizable nodes for filePath on the given
	// repo/branch, excluding container/sub-symbol kinds the projection hides.
	PromotedNodes(ctx context.Context, repoID, branch, filePath string) ([]Node, error)
	// SetShortSummary persists summary (already bounded to
	// domain.MaxShortSummaryRunes) for one node.
	SetShortSummary(ctx context.Context, repoID, branch, nodeID, summary string) error
}

// summarySchema constrains the model to a single JSON object carrying one
// "summary" string, so a chatty model cannot wrap the answer in prose.
var summarySchema = json.RawMessage(`{
  "type": "object",
  "properties": { "summary": { "type": "string" } },
  "required": ["summary"]
}`)

// summaryResponse is the parsed shape of the model output.
type summaryResponse struct {
	Summary string `json:"summary"`
}

// excludedKinds are container/sub-symbol kinds that get no LLM summary: a
// package or file summary is noise, and chunks/fields are sub-symbol fragments.
// Matches the duplicates surface's container exclusion.
var excludedKinds = map[string]struct{}{
	string(domain.KindPackage): {},
	string(domain.KindFile):    {},
	string(domain.KindChunk):   {},
	string(domain.KindModule):  {},
	string(domain.KindField):   {},
}

// summarizable reports whether a node kind should receive an LLM summary.
func summarizable(kind string) bool {
	_, excluded := excludedKinds[kind]
	return !excluded
}
