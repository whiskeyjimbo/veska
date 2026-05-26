package treesitter_test

// equivalence_test.go diffs the query-driven Go parser (solov2-1yev)
// against the hand-rolled walkers on a fixture corpus. Each phase of
// the rewrite expands what counts as "in scope" — phase 1 compares
// only KindFunction + KindPackage nodes since the query parser only
// produces those today. As later phases land (methods, types, calls,
// imports, ...) the in-scope filter widens and divergences become
// landmines for those phases to fix before promotion.

import (
	"context"
	"fmt"
	"sort"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/treesitter"
)

// phase1Kinds is the slice of node kinds the query parser currently
// emits. Widen this as later phases plug in more extractors; an
// extractor whose kind isn't listed here is exempt from the diff so we
// don't fail equivalence on un-ported behaviour.
var phase1Kinds = []domain.NodeKind{
	domain.KindFunction,
	domain.KindPackage,
}

// equivalenceFixtures is the corpus diff'd in both parsers. Kept small
// and deliberate — each fixture probes one shape (basic function,
// multiple declarations, syntax error, ...). When a fixture diverges,
// the failure message identifies which case so the offending phase
// knows where to look.
var equivalenceFixtures = []struct {
	name string
	src  string
}{
	{
		name: "single function",
		src: `package foo

func Hello() string { return "hi" }
`,
	},
	{
		name: "multiple functions",
		src: `package foo

func Add(a, b int) int { return a + b }
func Sub(a, b int) int { return a - b }
func unexported() {}
`,
	},
	{
		name: "function with signature args",
		src: `package foo

func Greet(name string, opts map[string]int) (string, error) {
	return "hi " + name, nil
}
`,
	},
	{
		name: "function inside syntax error",
		// Broken func declaration sandwiched between clean ones —
		// solov2-7nkm contract: the clean siblings still extract.
		src: `package foo

func Good() {}
func Broken( {
func AlsoGood() {}
`,
	},
}

func TestQueryParser_EquivalenceWithLegacy_Phase1(t *testing.T) {
	legacy := treesitter.NewGoParser()
	query := treesitter.NewGoQueryParser()
	for _, f := range equivalenceFixtures {
		t.Run(f.name, func(t *testing.T) {
			ctx := context.Background()
			legacyResult, err := legacy.ParseFile(ctx, "repo", "test.go", []byte(f.src))
			if err != nil {
				t.Fatalf("legacy parse: %v", err)
			}
			queryResult, err := query.ParseFile(ctx, "repo", "test.go", []byte(f.src))
			if err != nil {
				t.Fatalf("query parse: %v", err)
			}
			legacyNodes := keepKinds(legacyResult.Nodes, phase1Kinds)
			queryNodes := keepKinds(queryResult.Nodes, phase1Kinds)
			if diff := nodesDiff(legacyNodes, queryNodes); diff != "" {
				t.Errorf("node-set divergence (phase1 kinds):\n%s", diff)
			}
		})
	}
}

// keepKinds filters a node slice down to only the kinds in scope for
// the current rewrite phase. Order-preserving so the diff message
// stays interpretable.
func keepKinds(nodes []*domain.Node, kinds []domain.NodeKind) []*domain.Node {
	kindSet := map[domain.NodeKind]bool{}
	for _, k := range kinds {
		kindSet[k] = true
	}
	var out []*domain.Node
	for _, n := range nodes {
		if n != nil && kindSet[n.Kind] {
			out = append(out, n)
		}
	}
	return out
}

// nodesDiff returns "" when both node lists describe the same logical
// symbols. We compare (Kind, Name, Lines.Start, Lines.End, Exported) —
// the minimal fingerprint a downstream consumer reads off. NodeID
// equality is implied because nodeID() is a deterministic function of
// (repoID, path, kind, name) which all match when the visible fields
// match.
func nodesDiff(a, b []*domain.Node) string {
	type sig struct {
		Kind   string
		Name   string
		Start  int
		End    int
		Sig    string
		Exp    int // -1=nil, 0=false, 1=true
		HasRaw bool
	}
	toSigs := func(ns []*domain.Node) []sig {
		out := make([]sig, 0, len(ns))
		for _, n := range ns {
			s := sig{Kind: string(n.Kind), Name: n.Name}
			if n.Lines != nil {
				s.Start = n.Lines.Start
				s.End = n.Lines.End
			}
			if n.Signature != nil {
				s.Sig = *n.Signature
			}
			switch {
			case n.Exported == nil:
				s.Exp = -1
			case *n.Exported:
				s.Exp = 1
			default:
				s.Exp = 0
			}
			s.HasRaw = n.RawContent != nil
			out = append(out, s)
		}
		// Deterministic sort so a re-ordered emission between parsers
		// doesn't trip the diff.
		sort.Slice(out, func(i, j int) bool {
			if out[i].Kind != out[j].Kind {
				return out[i].Kind < out[j].Kind
			}
			return out[i].Name < out[j].Name
		})
		return out
	}
	la, lb := toSigs(a), toSigs(b)
	if len(la) != len(lb) {
		return formatNodesDiff("count mismatch", la, lb)
	}
	for i := range la {
		if la[i] != lb[i] {
			return formatNodesDiff("field divergence at index "+string(rune('0'+i)), la, lb)
		}
	}
	return ""
}

func formatNodesDiff(reason string, a, b any) string {
	return fmt.Sprintf("%s\n  legacy: %+v\n  query : %+v", reason, a, b)
}
