package search

import (
	"reflect"
	"testing"
)

// splitIdentifier
// The stems-aware matcher ( signal "b") needs to take an
// identifier and produce its lowercased subwords. Pinning the
// tokenisation independently from the rerank logic keeps the boundary
// rules explicit: camelCase, PascalCase, snake_case, dotted symbol
// paths, and acronym runs (HTTPServer → http, server) all need to land
// on the same subword set.

func TestSplitIdentifier_CamelCase(t *testing.T) {
	got := splitIdentifier("ParseConfig")
	want := []string{"parse", "config"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ParseConfig: got %v, want %v", got, want)
	}
}

func TestSplitIdentifier_SnakeCase(t *testing.T) {
	got := splitIdentifier("parse_config_file")
	want := []string{"parse", "config", "file"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parse_config_file: got %v, want %v", got, want)
	}
}

func TestSplitIdentifier_DottedSymbolPath(t *testing.T) {
	got := splitIdentifier("NoteStore.Save")
	want := []string{"note", "store", "save"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("NoteStore.Save: got %v, want %v", got, want)
	}
}

func TestSplitIdentifier_AcronymRun(t *testing.T) {
	// HTTPServer should split as http + server. Without acronym
	// handling we'd get [h,t,t,p,server] or [httpserver] - both wrong.
	got := splitIdentifier("HTTPServer")
	want := []string{"http", "server"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("HTTPServer: got %v, want %v", got, want)
	}
}

// definitionBoost

// TestRerank_DefinitionBoost_ExactTrailingMatch: an exact match on the
// trailing identifier of SymbolPath for a definitional Kind should
// outrank a substring-only match on a non-definitional Kind, even when
// raw scores were equal.
func TestRerank_DefinitionBoost_ExactTrailingMatch(t *testing.T) {
	in := []Result{
		// substring match ("save" appears in "SaveHandler") on a file kind
		{NodeID: "1", Score: 0.5, SymbolPath: "SaveHandler", FilePath: "/x/handler.go", Kind: "file"},
		// exact trailing match + definitional kind
		{NodeID: "2", Score: 0.5, SymbolPath: "NoteStore.Save", FilePath: "/x/store.go", Kind: "method"},
	}
	out := rerank(in, "save")
	if out[0].NodeID != "2" {
		t.Errorf("expected definitional exact match first, got %s (sym=%q kind=%q)",
			out[0].NodeID, out[0].SymbolPath, out[0].Kind)
	}
}

// identifierStems

// TestRerank_IdentifierStems_CamelMatch: "parse config" should boost
// both ParseConfig and configParser even though neither contains the
// literal phrase. The substring-based matcher already handles these
// (lowercased "parse" is in "parseconfig"), but pinning the case
// guards against regressions if we ever tighten substring matching.
func TestRerank_IdentifierStems_CamelMatch(t *testing.T) {
	in := []Result{
		{NodeID: "1", Score: 0.5, SymbolPath: "Server", FilePath: "/x/s.go", Kind: "type"},
		{NodeID: "2", Score: 0.5, SymbolPath: "ParseConfig", FilePath: "/x/c.go", Kind: "function"},
		{NodeID: "3", Score: 0.5, SymbolPath: "configParser", FilePath: "/x/c.go", Kind: "function"},
	}
	out := rerank(in, "parse config")
	// Top two should be the matches in some order; Server should be last.
	if out[2].NodeID != "1" {
		t.Errorf("Server should rank last, got order %s,%s,%s",
			out[0].NodeID, out[1].NodeID, out[2].NodeID)
	}
}

// TestRerank_IdentifierStems_NoPartialWordMatch: a query token "conf"
// must NOT match "config" via the stems matcher - stems are exact
// subword equality, not prefix. (Substring matcher could pick this up
// elsewhere; this test pins stem semantics, not the overall rerank.)
func TestRerank_IdentifierStems_NoPartialWordMatch(t *testing.T) {
	got := identifierStemMatches([]string{"conf"}, "ParseConfig", "")
	if got != 0 {
		t.Errorf("expected 0 stem matches for prefix 'conf' vs ParseConfig, got %d", got)
	}
}

// TestRerank_VerbSynonym_RegisterMatchesAdd guards: the
// junior-journey query "register subcommand" must surface
// Command.AddCommand above larger methods like Command.ExecuteC that
// merely *mention* subcommand handling. The lexical/embedding pipeline
// has no model of the "register/add/append/insert/install" synonym
// cluster API designers use; this signal injects that domain knowledge
// at the rerank layer so canonical "Add<Noun>" methods rise on
// "register/install <noun>" queries.
func TestRerank_VerbSynonym_RegisterMatchesAdd(t *testing.T) {
	in := []Result{
		// Large method that merely mentions subcommands in its body.
		{NodeID: "exec", Score: 0.020, SymbolPath: "Command.ExecuteC", FilePath: "/cobra/command.go", Kind: "method"},
		// Canonical answer - small method whose name is Add<Noun>.
		{NodeID: "add", Score: 0.011, SymbolPath: "Command.AddCommand", FilePath: "/cobra/command.go", Kind: "method"},
	}
	out := rerank(in, "register subcommand")
	if out[0].NodeID != "add" {
		t.Errorf("expected Command.AddCommand to rerank above Command.ExecuteC for 'register subcommand'; got order %s,%s",
			out[0].SymbolPath, out[1].SymbolPath)
	}
}

// TestRerank_VerbSynonym_LookupMatchesGet pins the synonym table on the
// retrieve-verb cluster (get / fetch / lookup / find / resolve) - a
// query "lookup config" should lift GetConfig above unrelated rows.
func TestRerank_VerbSynonym_LookupMatchesGet(t *testing.T) {
	in := []Result{
		{NodeID: "other", Score: 0.020, SymbolPath: "ParseInput", FilePath: "/x/parse.go", Kind: "function"},
		{NodeID: "get", Score: 0.011, SymbolPath: "GetConfig", FilePath: "/x/cfg.go", Kind: "function"},
	}
	out := rerank(in, "lookup config")
	if out[0].NodeID != "get" {
		t.Errorf("expected GetConfig to rerank above ParseInput for 'lookup config'; got order %s,%s",
			out[0].SymbolPath, out[1].SymbolPath)
	}
}

// TestRerank_VerbSynonym_NoFalsePositive guards that the synonym layer
// does NOT lift a candidate whose leading subword is unrelated to any
// query token - the boost is gated on the symbol's HEAD identifier (the
// verb position), not on substring presence anywhere.
func TestRerank_VerbSynonym_NoFalsePositive(t *testing.T) {
	in := []Result{
		// "add" appears mid-identifier as "ToAdd" suffix - not a verb position.
		{NodeID: "1", Score: 0.5, SymbolPath: "ItemsToAdd", FilePath: "/x/items.go", Kind: "function"},
		// A plain unrelated function.
		{NodeID: "2", Score: 0.5, SymbolPath: "Serialize", FilePath: "/x/io.go", Kind: "function"},
	}
	out := rerank(in, "register subcommand")
	// Both started at score 0.5 with no other rerank signals; expect
	// ItemsToAdd NOT lifted above Serialize (no leading verb match).
	// We assert by checking neither got a synonym bonus - equal final
	// scores preserve input order.
	if out[0].NodeID != "1" {
		t.Errorf("stable order broken (unexpected lift): got %s,%s", out[0].SymbolPath, out[1].SymbolPath)
	}
	if out[0].Score != out[1].Score {
		t.Errorf("synonym bonus fired on a non-verb-position match; scores %.4f vs %.4f", out[0].Score, out[1].Score)
	}
}

// fileCoherence

// TestRerank_FileCoherence_LiftsClusteredMatches: when multiple
// candidates come from the same file, all candidates in that file get
// a small bump. A clustered file should outrank an isolated file when
// raw scores are equal.
func TestRerank_FileCoherence_LiftsClusteredMatches(t *testing.T) {
	in := []Result{
		{NodeID: "1", Score: 0.5, SymbolPath: "Foo", FilePath: "/x/lonely.go", Kind: "function"},
		{NodeID: "2", Score: 0.49, SymbolPath: "Bar", FilePath: "/x/popular.go", Kind: "function"},
		{NodeID: "3", Score: 0.49, SymbolPath: "Baz", FilePath: "/x/popular.go", Kind: "function"},
		{NodeID: "4", Score: 0.49, SymbolPath: "Qux", FilePath: "/x/popular.go", Kind: "function"},
	}
	// Query with no name matches anywhere, so only coherence matters.
	out := rerank(in, "completelyunrelatedphrase")
	if out[0].FilePath != "/x/popular.go" {
		t.Errorf("expected /x/popular.go first via file coherence, got %s",
			out[0].FilePath)
	}
}

// noisePenalty

// TestRerank_NoisePenalty_TestFileDemotedBelowProdCode: when a query
// matches a name in both a *_test.go file and a real source file,
// the production hit should win.
func TestRerank_NoisePenalty_TestFileDemotedBelowProdCode(t *testing.T) {
	in := []Result{
		{NodeID: "1", Score: 0.5, SymbolPath: "TestNoteStore_Save", FilePath: "/x/store_test.go", Kind: "test"},
		{NodeID: "2", Score: 0.5, SymbolPath: "NoteStore.Save", FilePath: "/x/store.go", Kind: "method"},
	}
	out := rerank(in, "save")
	if out[0].NodeID != "2" {
		t.Errorf("expected prod-code hit first, test hit second; got order %s,%s",
			out[0].NodeID, out[1].NodeID)
	}
}

func TestRerank_NoisePenalty_PathPatterns(t *testing.T) {
	cases := []struct {
		path  string
		noisy bool
	}{
		{"/x/store.go", false},
		{"/x/store_test.go", true},
		{"/x/legacy/store.go", true},
		{"/x/examples/main.go", true},
		{"/x/vendor/lib/foo.go", true},
		{"/x/testdata/sample.go", true},
		{"/x/types.d.ts", true},
		{"/x/normal.ts", false},
	}
	for _, c := range cases {
		got := isNoisePath(c.path)
		if got != c.noisy {
			t.Errorf("isNoisePath(%q) = %v, want %v", c.path, got, c.noisy)
		}
	}
}

// integration: rerank composition

// TestRerank_EmptyQueryIsNoop replaces the prior boost_test empty-query
// case: the rerank pipeline as a whole must be a no-op on empty input.
func TestRerank_EmptyQueryIsNoop(t *testing.T) {
	in := []Result{
		{NodeID: "1", Score: 0.9, SymbolPath: "Z", FilePath: "/x/z.go", Kind: "function"},
		{NodeID: "2", Score: 0.1, SymbolPath: "A", FilePath: "/x/a.go", Kind: "function"},
	}
	out := rerank(in, "")
	if out[0].NodeID != "1" || out[1].NodeID != "2" {
		t.Errorf("empty query should preserve order; got %s,%s", out[0].NodeID, out[1].NodeID)
	}
}

// TestRerank_NoSignalsPreservesVectorOrder: when nothing matches and
// nothing is noisy, original vector-rank order is preserved (stable
// sort contract).
func TestRerank_NoSignalsPreservesVectorOrder(t *testing.T) {
	in := []Result{
		{NodeID: "1", Score: 0.9, SymbolPath: "Z", FilePath: "/x/z.go", Kind: "function"},
		{NodeID: "2", Score: 0.8, SymbolPath: "Y", FilePath: "/x/y.go", Kind: "function"},
		{NodeID: "3", Score: 0.7, SymbolPath: "X", FilePath: "/x/x.go", Kind: "function"},
	}
	out := rerank(in, "totallyunrelatedwordzzz")
	for i, want := range []string{"1", "2", "3"} {
		if out[i].NodeID != want {
			t.Errorf("rank %d: got %s, want %s", i, out[i].NodeID, want)
		}
	}
}
