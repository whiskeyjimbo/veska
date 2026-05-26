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
//
// Phase 2 widened the set to include method / struct / interface /
// type / variable — the query parser now emits all five.
var phase1Kinds = []domain.NodeKind{
	domain.KindFunction,
	domain.KindPackage,
	domain.KindMethod,
	domain.KindStruct,
	domain.KindInterface,
	domain.KindType,
	domain.KindVariable,
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
	{
		name: "methods with pointer and value receivers",
		src: `package foo

type Counter struct{ n int }

func (c *Counter) Inc() { c.n++ }
func (c Counter) Value() int { return c.n }
func (c *Counter) reset() { c.n = 0 }
`,
	},
	{
		name: "type kinds: struct, interface, alias",
		src: `package foo

type Point struct {
	X, Y float64
}

type Writer interface {
	Write(p []byte) (n int, err error)
	Close() error
}

type StringList []string

type ID = int
`,
	},
	{
		name: "top-level vars and consts",
		src: `package foo

var rootCmd = "demo"

var (
	verbose bool
	logFile string
	_       = 42
)

const (
	Default = "x"
	max     = 100
)
`,
	},
	{
		name: "mixed declarations",
		src: `package mixed

import "fmt"

type Server struct {
	addr string
}

func (s *Server) Start() error { return nil }

var defaultServer = &Server{addr: ":80"}

const port = 8080

func Run() {
	fmt.Println("hi")
}
`,
	},
	{
		name: "in-file calls",
		// identifier-form calls bind directly to the in-file symbol map.
		src: `package foo

func one() int { return 1 }
func two() int { return one() + one() }
func three() int { return two() + one() }
`,
	},
	{
		name: "receiver method calls",
		// s.foo() inside a method on *Server binds to "Server.foo".
		src: `package foo

type Server struct{}

func (s *Server) helper() string { return "" }
func (s *Server) Handle() string {
	return s.helper()
}
`,
	},
	{
		name: "package-qualified calls",
		// pkg.X becomes an UnresolvedCall with PkgQualifier; the import
		// map tells promotion which module to resolve against.
		src: `package main

import (
	"fmt"
	"github.com/acme/mycli/cmd"
	flag "github.com/spf13/pflag"
	_ "embed"
)

func main() {
	cmd.Execute()
	flag.Parse()
	fmt.Println("hi")
}
`,
	},
	{
		name: "mixed calls + nested control flow",
		// nested if/for/switch — calls.scm should match call_expression
		// at any depth, not just top-of-body.
		src: `package foo

func cond() bool { return true }
func loop() {}
func work() {
	if cond() {
		for i := 0; i < 3; i++ {
			loop()
		}
	}
}
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
			// Phase 3a adds import + edge + unresolved-call diffs to the
			// harness. Each is filtered by the same phase scope: we
			// only compare what the query parser claims to handle today
			// so partial-phase commits don't fail equivalence on
			// un-ported behaviour.
			if diff := importsDiff(legacyResult.Imports, queryResult.Imports); diff != "" {
				t.Errorf("imports divergence:\n%s", diff)
			}
			if diff := edgesDiff(legacyResult.Edges, queryResult.Edges); diff != "" {
				t.Errorf("edges divergence:\n%s", diff)
			}
			if diff := unresolvedDiff(legacyResult.UnresolvedCalls, queryResult.UnresolvedCalls); diff != "" {
				t.Errorf("unresolved-calls divergence:\n%s", diff)
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

// importsDiff compares the alias → import-path map both parsers
// produce. Order-independent (it's a map), missing-on-one-side is
// surfaced explicitly. Both nil and empty are treated as equivalent
// so "no imports in this fixture" doesn't trip a false positive.
func importsDiff(a, b map[string]string) string {
	if len(a) == 0 && len(b) == 0 {
		return ""
	}
	keys := map[string]bool{}
	for k := range a {
		keys[k] = true
	}
	for k := range b {
		keys[k] = true
	}
	type entry struct{ K, A, B string }
	var diffs []entry
	for k := range keys {
		if a[k] != b[k] {
			diffs = append(diffs, entry{k, a[k], b[k]})
		}
	}
	if len(diffs) == 0 {
		return ""
	}
	sort.Slice(diffs, func(i, j int) bool { return diffs[i].K < diffs[j].K })
	return fmt.Sprintf("%d import entr(y/ies) differ:\n  %+v", len(diffs), diffs)
}

// edgesDiff compares result.Edges between legacy and query parsers.
// Edges are unordered, so we hash each by (Src, Tgt, Kind) and compare
// the multisets. Different counts of the same edge-shape do diverge —
// the legacy parser dedups within a caller, and the query parser
// should match that. A single divergence prints both edge sets in
// canonical order so the difference is visually obvious.
func edgesDiff(a, b []*domain.Edge) string {
	type key struct{ Src, Tgt, Kind string }
	count := func(es []*domain.Edge) map[key]int {
		out := map[key]int{}
		for _, e := range es {
			if e == nil {
				continue
			}
			out[key{string(e.Src), string(e.Tgt), string(e.Kind)}]++
		}
		return out
	}
	ca, cb := count(a), count(b)
	keys := map[key]bool{}
	for k := range ca {
		keys[k] = true
	}
	for k := range cb {
		keys[k] = true
	}
	type entry struct {
		K    key
		A, B int
	}
	var diffs []entry
	for k := range keys {
		if ca[k] != cb[k] {
			diffs = append(diffs, entry{k, ca[k], cb[k]})
		}
	}
	if len(diffs) == 0 {
		return ""
	}
	sort.Slice(diffs, func(i, j int) bool {
		if diffs[i].K.Kind != diffs[j].K.Kind {
			return diffs[i].K.Kind < diffs[j].K.Kind
		}
		if diffs[i].K.Src != diffs[j].K.Src {
			return diffs[i].K.Src < diffs[j].K.Src
		}
		return diffs[i].K.Tgt < diffs[j].K.Tgt
	})
	return fmt.Sprintf("%d edge(s) differ (counts shown as legacy/query):\n  %+v", len(diffs), diffs)
}

// unresolvedDiff compares UnresolvedCalls slices. Each UnresolvedCall
// is unique per (CallerID, CalleeName, PkgQualifier, IsMethodCall);
// dedup on that key for set comparison.
func unresolvedDiff(a, b []domain.UnresolvedCall) string {
	type key struct {
		Caller, Callee, Pkg string
		Method              bool
	}
	toSet := func(ucs []domain.UnresolvedCall) map[key]bool {
		out := map[key]bool{}
		for _, u := range ucs {
			out[key{string(u.CallerID), u.CalleeName, u.PkgQualifier, u.IsMethodCall}] = true
		}
		return out
	}
	sa, sb := toSet(a), toSet(b)
	var only []key
	for k := range sa {
		if !sb[k] {
			only = append(only, k)
		}
	}
	var only2 []key
	for k := range sb {
		if !sa[k] {
			only2 = append(only2, k)
		}
	}
	if len(only) == 0 && len(only2) == 0 {
		return ""
	}
	return fmt.Sprintf("legacy-only: %+v\n  query-only: %+v", only, only2)
}
