// Unit tests for the build-tag-free projection helpers. These run under
// standard `go test` (no Ollama, no `eval` tag) and assert the core
// solov2-7ma property: the variant selector produces DIFFERENT embed
// text for different variants, and the corpus is built via domain.EmbedText.
package recallprojection

import (
	"strings"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/tools/loadtest/synthcorpus"
)

func TestBuildProjectionCorpusShape(t *testing.T) {
	src := synthcorpus.GenerateSemanticCorpus(2)
	corpus := BuildProjectionCorpus(src)

	if corpus.Clusters != src.Clusters {
		t.Fatalf("clusters: got %d want %d", corpus.Clusters, src.Clusters)
	}
	if len(corpus.Nodes) != len(src.Nodes) {
		t.Fatalf("node count: got %d want %d", len(corpus.Nodes), len(src.Nodes))
	}
	if len(corpus.CenterQueries) != len(src.CenterQueries) {
		t.Fatalf("center queries: got %d want %d", len(corpus.CenterQueries), len(src.CenterQueries))
	}
	for i, n := range corpus.Nodes {
		if n.NodeID != src.Nodes[i].NodeID {
			t.Fatalf("node %d id: got %q want %q", i, n.NodeID, src.Nodes[i].NodeID)
		}
		if n.Input.SymbolPath == "" || n.Input.Kind == "" {
			t.Fatalf("node %d: empty structural projection field: %+v", i, n.Input)
		}
		if n.Input.Signature == "" || n.Input.Snippet == "" {
			t.Fatalf("node %d: empty enrichment field: %+v", i, n.Input)
		}
	}
}

func TestTruthByClusterMatchesSource(t *testing.T) {
	src := synthcorpus.GenerateSemanticCorpus(3)
	corpus := BuildProjectionCorpus(src)

	got := corpus.TruthByCluster()
	want := src.TruthByCluster()
	if len(got) != len(want) {
		t.Fatalf("truth length: got %d want %d", len(got), len(want))
	}
	for k := range want {
		if len(got[k]) != len(want[k]) {
			t.Fatalf("cluster %d size: got %d want %d", k, len(got[k]), len(want[k]))
		}
		for id := range want[k] {
			if _, ok := got[k][id]; !ok {
				t.Fatalf("cluster %d: missing %q in projection truth", k, id)
			}
		}
	}
}

// TestVariantSelectorProducesDistinctText is the load-bearing assertion:
// the four variants must yield genuinely different embed text, otherwise
// a sweep cannot move the recall number.
func TestVariantSelectorProducesDistinctText(t *testing.T) {
	src := synthcorpus.GenerateSemanticCorpus(1)
	corpus := BuildProjectionCorpus(src)
	node := corpus.Nodes[0]

	base := node.EmbedText(domain.EmbedVariantBaseline)
	sig := node.EmbedText(domain.EmbedVariantSignature)
	snip := node.EmbedText(domain.EmbedVariantSnippet)
	both := node.EmbedText(domain.EmbedVariantBoth)

	seen := map[string]string{base: "baseline", sig: "+signature", snip: "+snippet", both: "+both"}
	if len(seen) != 4 {
		t.Fatalf("variants did not all produce distinct text: %v", seen)
	}

	// Baseline must be a prefix of every enrichment variant — enrichment
	// only appends, it never rewrites the production projection.
	for name, text := range map[string]string{"+signature": sig, "+snippet": snip, "+both": both} {
		if !strings.HasPrefix(text, base) {
			t.Fatalf("%s does not extend baseline:\n base=%q\n  got=%q", name, base, text)
		}
	}
	// +both must carry both the signature and the snippet content.
	if !strings.Contains(both, node.Input.Signature) {
		t.Fatalf("+both missing signature content: %q", both)
	}
	if !strings.Contains(both, node.Input.Snippet) {
		t.Fatalf("+both missing snippet content: %q", both)
	}
}

// TestProjectionMatchesProductionBaseline guards the contract that the
// baseline variant is exactly the production FetchPending projection.
func TestProjectionMatchesProductionBaseline(t *testing.T) {
	in := domain.EmbedTextInput{
		Kind:       "function",
		SymbolPath: "pkg.Thing",
		FilePath:   "pkg/thing.go",
		Language:   "go",
		Signature:  "func Thing() error",
		Snippet:    "return nil",
	}
	// Baseline must NOT include signature or snippet.
	base := domain.EmbedText(in, domain.EmbedVariantBaseline)
	if want := "function pkg.Thing pkg/thing.go go"; base != want {
		t.Fatalf("baseline projection: got %q want %q", base, want)
	}
}

func TestVariantByName(t *testing.T) {
	cases := map[string]domain.EmbedTextVariant{
		"":           domain.EmbedVariantBaseline,
		"baseline":   domain.EmbedVariantBaseline,
		"+signature": domain.EmbedVariantSignature,
		"signature":  domain.EmbedVariantSignature,
		"+snippet":   domain.EmbedVariantSnippet,
		"+both":      domain.EmbedVariantBoth,
		"BOTH":       domain.EmbedVariantBoth,
		"garbage":    domain.EmbedVariantBaseline,
	}
	for name, want := range cases {
		if got := VariantByName(name); got != want {
			t.Fatalf("VariantByName(%q): got %v want %v", name, got, want)
		}
		// Round-trip through String() for the canonical names.
		if want != domain.EmbedVariantBaseline && VariantByName(want.String()) != want {
			t.Fatalf("round-trip failed for %v", want)
		}
	}
}
