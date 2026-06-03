// Package recallprojection is the eval harness that makes embed-text
// PROJECTION changes measurable for recall sweeps.
//
// Background : the recall harness in tools/loadtest/recall
// embeds the synthetic corpus Text field directly. It therefore does not
// exercise the production FetchPending embed-text projection
// (domain.EmbedText), so swapping projection variants — folding the
// symbol signature and/or a code snippet into the embed text — does not
// move its measured recall number.
//
// This harness closes that gap: it builds the recall corpus from
// node-shaped projection inputs run through domain.EmbedText, the SAME
// function the production sqlite adapter uses. A projection variant is
// selectable (baseline / +signature / +snippet / +both) so a
// reference-laptop run can sweep them against the m3.03 recall fixture.
//
// The pure (build-tag-free) helpers live here so they compile and unit-
// test under standard `go test`/`go vet`; the end-to-end Ollama-backed
// driver is gated by the `eval` build tag in projection_eval_test.go.
package recallprojection

import (
	"fmt"
	"strings"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/tools/loadtest/synthcorpus"
)

// ProjectionNode is a corpus node carried as a node-shaped projection
// input plus its ground-truth cluster. It mirrors the fields the
// production FetchPending join selects (kind, symbol_path, file_path,
// language) and adds the optional enrichment fields (signature, snippet)
// the sweep evaluates.
type ProjectionNode struct {
	NodeID  string
	Cluster int
	Input   domain.EmbedTextInput
}

// EmbedText returns the projection of this node under the given variant,
// produced by the SAME domain.EmbedText function the production sqlite
// adapter calls. This is the load-bearing line of the harness: the
// recall corpus is embedded via this output, so a variant change is what
// the recall number actually measures.
func (n ProjectionNode) EmbedText(variant domain.EmbedTextVariant) string {
	return domain.EmbedText(n.Input, variant)
}

// ProjectionCorpus is the node-shaped recall corpus: projection nodes
// plus one center query per cluster (carried verbatim from the synthetic
// corpus the structural ground truth is built on).
type ProjectionCorpus struct {
	Clusters      int
	Nodes         []ProjectionNode
	CenterQueries []string
}

// BuildProjectionCorpus converts a synthcorpus.Corpus into a node-shaped
// projection corpus. Each synthetic node's semantic Text is decomposed
// into the projection fields:
//
//   - Kind / SymbolPath / FilePath / Language — the structural identifiers
//     the production baseline projection joins. SymbolPath and FilePath
//     come straight from the synthetic node; Language is fixed to "go".
//   - Signature — a synthesized symbol signature that paraphrases the
//     node's semantic content, consumed only by the +signature / +both
//     variants.
//   - Snippet — a synthesized one-line code snippet that restates the
//     semantic content, consumed only by the +snippet / +both variants.
//
// The baseline projection therefore carries only the structural
// identifiers; the enrichment variants additionally carry the semantic
// content, so a real embedding model produces measurably different
// vectors per variant and the recall sweep is meaningful.
func BuildProjectionCorpus(src synthcorpus.Corpus) ProjectionCorpus {
	out := ProjectionCorpus{
		Clusters:      src.Clusters,
		Nodes:         make([]ProjectionNode, 0, len(src.Nodes)),
		CenterQueries: append([]string(nil), src.CenterQueries...),
	}
	for _, n := range src.Nodes {
		out.Nodes = append(out.Nodes, ProjectionNode{
			NodeID:  n.NodeID,
			Cluster: n.Cluster,
			Input: domain.EmbedTextInput{
				Kind:       orDefault(n.Kind, "function"),
				SymbolPath: n.SymbolPath,
				FilePath:   n.FilePath,
				Language:   "go",
				Signature:  synthSignature(n),
				Snippet:    synthSnippet(n),
			},
		})
	}
	return out
}

// TruthByCluster returns the ground-truth NodeID set per cluster, matching
// synthcorpus.Corpus.TruthByCluster so the recall math is identical.
func (c ProjectionCorpus) TruthByCluster() []map[string]struct{} {
	out := make([]map[string]struct{}, c.Clusters)
	for k := range out {
		out[k] = make(map[string]struct{})
	}
	for _, n := range c.Nodes {
		out[n.Cluster][n.NodeID] = struct{}{}
	}
	return out
}

// synthSignature builds a function-signature-shaped string that
// paraphrases the node's semantic text. The synthetic corpus Text is a
// short list of topic phrases; the signature turns the first few into a
// pseudo-parameter list so the +signature variant carries semantic
// content the baseline structural projection does not.
func synthSignature(n synthcorpus.SyntheticNode) string {
	phrases := splitPhrases(n.Text)
	params := make([]string, 0, len(phrases))
	for i, p := range phrases {
		if i >= 4 {
			break
		}
		params = append(params, identifierize(p))
	}
	short := lastSegment(n.SymbolPath)
	return fmt.Sprintf("func %s(%s) error", short, strings.Join(params, ", "))
}

// synthSnippet builds a one-line code-snippet-shaped string that restates
// the node's semantic text, so the +snippet variant carries semantic
// content distinct from the signature paraphrase.
func synthSnippet(n synthcorpus.SyntheticNode) string {
	return fmt.Sprintf("// %s\nreturn handle(%q)", n.Text, n.Text)
}

// splitPhrases splits a synthetic node Text ("phrase a. phrase b.") into
// trimmed, non-empty phrases.
func splitPhrases(text string) []string {
	out := make([]string, 0, 8)
	for p := range strings.SplitSeq(text, ".") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// identifierize collapses a phrase into a camelCase-ish identifier so it
// reads like a parameter name.
func identifierize(phrase string) string {
	words := strings.Fields(phrase)
	var b strings.Builder
	for i, w := range words {
		if i == 0 {
			b.WriteString(strings.ToLower(w))
			continue
		}
		//lint:ignore SA1019 strings.Title fine for ASCII-only synthetic input.
		b.WriteString(strings.Title(strings.ToLower(w))) //nolint:staticcheck // ASCII-only synthetic input
	}
	if b.Len() == 0 {
		return "arg"
	}
	return b.String()
}

// lastSegment returns the final dot-separated segment of a symbol path.
func lastSegment(symbolPath string) string {
	if i := strings.LastIndexByte(symbolPath, '.'); i >= 0 && i+1 < len(symbolPath) {
		return symbolPath[i+1:]
	}
	if symbolPath == "" {
		return "fn"
	}
	return symbolPath
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// VariantByName maps an env-knob string to a domain.EmbedTextVariant.
// Unknown / empty values fall back to the baseline variant. The accepted
// names match domain.EmbedTextVariant.String() so a report row round-trips.
func VariantByName(name string) domain.EmbedTextVariant {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "+signature", "signature":
		return domain.EmbedVariantSignature
	case "+snippet", "snippet":
		return domain.EmbedVariantSnippet
	case "+both", "both":
		return domain.EmbedVariantBoth
	default:
		return domain.EmbedVariantBaseline
	}
}

// AllVariants is the full sweep set, in report order.
var AllVariants = []domain.EmbedTextVariant{
	domain.EmbedVariantBaseline,
	domain.EmbedVariantSignature,
	domain.EmbedVariantSnippet,
	domain.EmbedVariantBoth,
}
