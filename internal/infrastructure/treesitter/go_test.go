package treesitter_test

import (
	"context"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/treesitter"
)

const repoID = "test-repo"
const filePath = "pkg/foo/foo.go"

func TestParseFile_FunctionDeclaration(t *testing.T) {
	src := []byte(`package foo

func Add(a, b int) int {
	return a + b
}
`)
	p := treesitter.NewGoParser()
	result, err := p.ParseFile(context.Background(), repoID, filePath, src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	fn := findNodeByName(result.Nodes, "Add")
	if fn == nil {
		t.Fatal("expected a node named 'Add', got none")
		return
	}
	if fn.Kind != domain.KindFunction {
		t.Errorf("expected KindFunction, got %q", fn.Kind)
	}
	if fn.Lines == nil {
		t.Fatal("expected Lines to be set")
		return
	}
	if fn.Lines.Start != 3 {
		t.Errorf("expected Start=3, got %d", fn.Lines.Start)
	}
}

// TestParseFile_TopLevelVarDecl guards solov2-b7wt: top-level var
// declarations (the dominant API-surface pattern in cobra CLIs) must be
// extracted as KindVariable nodes. Without this, eng_find_symbol returns
// empty for `rootCmd` etc.
func TestParseFile_TopLevelVarDecl(t *testing.T) {
	src := []byte(`package main

import "github.com/spf13/cobra"

var rootCmd = &cobra.Command{
	Use:   "tool",
	Short: "demo",
}

var (
	verbose bool
	logFile string
)

const _hidden = "skip me"
`)
	p := treesitter.NewGoParser()
	result, err := p.ParseFile(context.Background(), repoID, filePath, src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := []string{"rootCmd", "verbose", "logFile"}
	for _, name := range want {
		n := findNodeByName(result.Nodes, name)
		if n == nil {
			t.Errorf("expected var %q to be extracted; got none", name)
			continue
		}
		if n.Kind != domain.KindVariable {
			t.Errorf("var %q: kind = %q, want %q", name, n.Kind, domain.KindVariable)
		}
	}
	// Raw content must include the cobra struct body so semantic search
	// indexes the Use:/Short: strings.
	if n := findNodeByName(result.Nodes, "rootCmd"); n != nil && n.RawContent == nil {
		t.Errorf("rootCmd.RawContent must be populated for semantic-search visibility")
	}
}

func TestParseFile_MethodDeclaration(t *testing.T) {
	src := []byte(`package foo

type Counter struct{ n int }

func (c Counter) Inc() {
	c.n++
}
`)
	p := treesitter.NewGoParser()
	result, err := p.ParseFile(context.Background(), repoID, filePath, src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	method := findNodeByName(result.Nodes, "Counter.Inc")
	if method == nil {
		t.Fatal("expected a node named 'Counter.Inc', got none")
		return
	}
	if method.Kind != domain.KindMethod {
		t.Errorf("expected KindMethod, got %q", method.Kind)
	}
}

func TestParseFile_StructType(t *testing.T) {
	src := []byte(`package foo

type Point struct {
	X, Y float64
}
`)
	p := treesitter.NewGoParser()
	result, err := p.ParseFile(context.Background(), repoID, filePath, src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	node := findNodeByName(result.Nodes, "Point")
	if node == nil {
		t.Fatal("expected a node named 'Point', got none")
		return
	}
	if node.Kind != domain.KindStruct {
		t.Errorf("expected KindStruct, got %q", node.Kind)
	}
}

func TestParseFile_InterfaceType(t *testing.T) {
	src := []byte(`package foo

type Writer interface {
	Write(p []byte) (n int, err error)
}
`)
	p := treesitter.NewGoParser()
	result, err := p.ParseFile(context.Background(), repoID, filePath, src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	node := findNodeByName(result.Nodes, "Writer")
	if node == nil {
		t.Fatal("expected a node named 'Writer', got none")
		return
	}
	if node.Kind != domain.KindInterface {
		t.Errorf("expected KindInterface, got %q", node.Kind)
	}
}

func TestParseFile_CallsEdge(t *testing.T) {
	src := []byte(`package foo

func greet() string {
	return hello()
}

func hello() string {
	return "hello"
}
`)
	p := treesitter.NewGoParser()
	result, err := p.ParseFile(context.Background(), repoID, filePath, src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	greetNode := findNodeByName(result.Nodes, "greet")
	helloNode := findNodeByName(result.Nodes, "hello")
	if greetNode == nil || helloNode == nil {
		t.Fatalf("expected greet and hello nodes, got nodes: %v", nodeNames(result.Nodes))
		return
	}

	edge := findEdge(result.Edges, greetNode.ID, helloNode.ID, domain.EdgeCalls)
	if edge == nil {
		t.Errorf("expected CALLS edge from greet -> hello, none found")
	}
}

// TestParseFile_ErrorRecovery pins solov2-7nkm: a syntax error in one
// declaration must not erase the file's other symbols. The clean function is
// still extracted, a ParseFailure is reported, and the broken declaration is
// skipped.
func TestParseFile_ErrorRecovery(t *testing.T) {
	src := []byte(`package foo

func Good() string { return "ok" }

func Broken( {  // syntax error: unclosed param list

func AlsoGood() int { return 1 }
`)
	p := treesitter.NewGoParser()
	result, err := p.ParseFile(context.Background(), repoID, filePath, src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Failures) == 0 {
		t.Error("expected a ParseFailure for the broken declaration")
	}
	if findNodeByName(result.Nodes, "Good") == nil {
		t.Errorf("clean func Good was discarded; nodes: %v", nodeNames(result.Nodes))
	}
}

// TestParseFile_ImportsAndQualifiedCalls pins solov2-xc51.1: the parser must
// surface the file's import map and capture package-qualified calls
// (cmd.Execute()) as UnresolvedCalls carrying a PkgQualifier — the foundation
// for cross-package CALLS resolution at promotion.
func TestParseFile_ImportsAndQualifiedCalls(t *testing.T) {
	src := []byte(`package main

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
`)
	p := treesitter.NewGoParser()
	result, err := p.ParseFile(context.Background(), repoID, filePath, src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Import map: aliased + unaliased; blank import excluded.
	if got := result.Imports["cmd"]; got != "github.com/acme/mycli/cmd" {
		t.Errorf("imports[cmd] = %q, want github.com/acme/mycli/cmd", got)
	}
	if got := result.Imports["flag"]; got != "github.com/spf13/pflag" {
		t.Errorf("imports[flag] = %q (alias), want github.com/spf13/pflag", got)
	}
	if got := result.Imports["fmt"]; got != "fmt" {
		t.Errorf("imports[fmt] = %q, want fmt", got)
	}
	if _, ok := result.Imports["embed"]; ok {
		t.Errorf("blank import should not appear in import map: %v", result.Imports)
	}

	// Qualified calls captured with PkgQualifier.
	wantQ := map[string]string{"Execute": "cmd", "Parse": "flag", "Println": "fmt"}
	gotQ := map[string]string{}
	for _, uc := range result.UnresolvedCalls {
		if uc.PkgQualifier != "" {
			gotQ[uc.CalleeName] = uc.PkgQualifier
		}
	}
	for callee, pkg := range wantQ {
		if gotQ[callee] != pkg {
			t.Errorf("qualified call %s: qualifier = %q, want %q (all: %v)", callee, gotQ[callee], pkg, gotQ)
		}
	}
}

// TestParseFile_ChainedSelectorMethodCall covers solov2-9rc2 phase A:
// a local variable assigned from a package-qualified constructor whose
// subsequent method calls were previously dropped by the parser
// (`g := greetlib.New(...); g.Hello(...)` only produced Run→New, not
// Run→Hello). The parser must now emit an UnresolvedCall with
// IsMethodCall=true so promotion can bind by method name within the
// originating package.
func TestParseFile_ChainedSelectorMethodCall(t *testing.T) {
	src := []byte(`package runner

import "github.com/jrose/greetlib"

func Run(name string) string {
	g := greetlib.New("Hello")
	return g.Hello(name)
}
`)
	p := treesitter.NewGoParser()
	result, err := p.ParseFile(context.Background(), repoID, filePath, src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var sawNew, sawHello bool
	var helloIsMethod bool
	for _, uc := range result.UnresolvedCalls {
		switch {
		case uc.PkgQualifier == "greetlib" && uc.CalleeName == "New" && !uc.IsMethodCall:
			sawNew = true
		case uc.PkgQualifier == "greetlib" && uc.CalleeName == "Hello":
			sawHello = true
			helloIsMethod = uc.IsMethodCall
		}
	}
	if !sawNew {
		t.Errorf("UnresolvedCalls missing plain pkg call greetlib.New: %+v", result.UnresolvedCalls)
	}
	if !sawHello {
		t.Errorf("UnresolvedCalls missing chained-selector call g.Hello (greetlib): %+v", result.UnresolvedCalls)
	}
	if sawHello && !helloIsMethod {
		t.Errorf("g.Hello must be flagged IsMethodCall=true so the resolver looks up methods by name in greetlib; got false")
	}
}

// TestParseFile_StructFieldMethodCall covers solov2-9rc2 phase E v1:
// `s.field.Method()` where the field is declared with a same-package
// concrete struct type resolves directly to ReceiverType.Method via the
// file's symbol map. This is the hexagonal/DI shape that the original
// epic acceptance ("Promoter.Promote ≥ 7 edges") was filed against.
func TestParseFile_StructFieldMethodCall(t *testing.T) {
	src := []byte(`package app

type Staging struct{}

func (s *Staging) Snapshot() string { return "" }

type Promoter struct {
	staging *Staging
}

func (p *Promoter) Promote() string {
	return p.staging.Snapshot()
}
`)
	p := treesitter.NewGoParser()
	result, err := p.ParseFile(context.Background(), repoID, filePath, src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Find Promote -> Staging.Snapshot edge in the resolved edges (not in
	// UnresolvedCalls — same-file/same-package resolves directly).
	var promoteID, snapshotID string
	for _, n := range result.Nodes {
		switch n.Name {
		case "Promoter.Promote":
			promoteID = string(n.ID)
		case "Staging.Snapshot":
			snapshotID = string(n.ID)
		}
	}
	if promoteID == "" || snapshotID == "" {
		t.Fatalf("expected Promoter.Promote and Staging.Snapshot nodes, got %+v", result.Nodes)
	}
	found := false
	for _, e := range result.Edges {
		if e.Kind == domain.EdgeCalls && string(e.Src) == promoteID && string(e.Tgt) == snapshotID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("want CALLS edge Promoter.Promote -> Staging.Snapshot (same-package struct field method); have edges: %+v", result.Edges)
	}
}

// TestParseFile_InterfaceMethodsAsNodes covers solov2-9rc2 phase E v2:
// every method declared on an interface type must surface as its own
// KindMethod node named IfaceName.MethodName, so chained-selector calls
// through interface-typed fields can resolve to a concrete graph node.
func TestParseFile_InterfaceMethodsAsNodes(t *testing.T) {
	src := []byte(`package ports

type AuditWriter interface {
	Write(ctx string) error
	Close() error
}
`)
	p := treesitter.NewGoParser()
	result, err := p.ParseFile(context.Background(), repoID, filePath, src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := map[string]bool{"AuditWriter.Write": false, "AuditWriter.Close": false}
	for _, n := range result.Nodes {
		if n.Kind == domain.KindMethod {
			if _, ok := want[n.Name]; ok {
				want[n.Name] = true
			}
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("interface method node %q missing from result; have nodes: %+v", name, result.Nodes)
		}
	}
}

// TestParseFile_SamePackageInterfaceFieldCall covers solov2-9rc2 phase E
// v2 same-package path: `p.store.Promote()` where store is a field of an
// interface type declared in the same package resolves to the interface
// method node IfaceName.Method via the file's symbol map.
func TestParseFile_SamePackageInterfaceFieldCall(t *testing.T) {
	src := []byte(`package app

type PromotionStore interface {
	Promote() error
}

type Promoter struct {
	store PromotionStore
}

func (p *Promoter) Promote() error {
	return p.store.Promote()
}
`)
	p := treesitter.NewGoParser()
	result, err := p.ParseFile(context.Background(), repoID, filePath, src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var promoterPromoteID, ifacePromoteID string
	for _, n := range result.Nodes {
		switch n.Name {
		case "Promoter.Promote":
			promoterPromoteID = string(n.ID)
		case "PromotionStore.Promote":
			ifacePromoteID = string(n.ID)
		}
	}
	if promoterPromoteID == "" || ifacePromoteID == "" {
		t.Fatalf("expected Promoter.Promote and PromotionStore.Promote nodes; got %+v", result.Nodes)
	}
	found := false
	for _, e := range result.Edges {
		if e.Kind == domain.EdgeCalls && string(e.Src) == promoterPromoteID && string(e.Tgt) == ifacePromoteID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("want CALLS edge Promoter.Promote -> PromotionStore.Promote through struct field; have edges: %+v", result.Edges)
	}
}

// TestParseFile_CrossPackageInterfaceFieldCall covers solov2-9rc2 phase E v2
// cross-package path: `p.audit.Write()` where audit is an interface-typed
// field from another package emits an UnresolvedCall with PkgQualifier
// and IsMethodCall=true, which Phase C/D resolve to the interface method
// node in the imported package.
func TestParseFile_CrossPackageInterfaceFieldCall(t *testing.T) {
	src := []byte(`package app

import "github.com/example/ports"

type Promoter struct {
	audit ports.AuditWriter
}

func (p *Promoter) Promote() {
	p.audit.Write("event")
}
`)
	p := treesitter.NewGoParser()
	result, err := p.ParseFile(context.Background(), repoID, filePath, src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var saw bool
	for _, uc := range result.UnresolvedCalls {
		if uc.PkgQualifier == "ports" && uc.CalleeName == "Write" && uc.IsMethodCall {
			saw = true
			break
		}
	}
	if !saw {
		t.Errorf("cross-package interface field call p.audit.Write must emit UnresolvedCall{Pkg:ports, Callee:Write, IsMethodCall:true}; got %+v", result.UnresolvedCalls)
	}
}

// TestParseFile_PromoterShape_ChainedFieldCalls is a regression-shape
// test mirroring the original solov2-9rc2 epic acceptance: a hexagonal
// Promoter with multiple chained-selector calls through struct fields.
// Each p.X.M() chain must produce an UnresolvedCall or an in-file edge.
func TestParseFile_PromoterShape_ChainedFieldCalls(t *testing.T) {
	src := []byte(`package app

import "github.com/example/ports"

type Staging struct{}
func (s *Staging) Snapshot() string { return "" }
func (s *Staging) Delete() {}

type PromotionStore interface { Promote() error }
type CheckRunner interface { Run() }

type Promoter struct {
	staging *Staging
	store   PromotionStore
	checks  CheckRunner
	audit   ports.AuditWriter
}

func (p *Promoter) Promote() {
	_ = p.staging.Snapshot()
	_ = p.store.Promote()
	p.checks.Run()
	_ = p.audit.Write("event")
	p.staging.Delete()
}
`)
	p := treesitter.NewGoParser()
	result, err := p.ParseFile(context.Background(), repoID, filePath, src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Map nodes by name.
	idByName := map[string]string{}
	for _, n := range result.Nodes {
		idByName[n.Name] = string(n.ID)
	}
	promoterID := idByName["Promoter.Promote"]
	if promoterID == "" {
		t.Fatalf("Promoter.Promote node missing")
	}

	// Same-package field calls must land as in-file edges.
	wantEdgeTargets := []string{
		"Staging.Snapshot",       // *Staging field
		"Staging.Delete",         // *Staging field (second call site)
		"PromotionStore.Promote", // same-pkg interface field
		"CheckRunner.Run",        // same-pkg interface field
	}
	for _, target := range wantEdgeTargets {
		targetID := idByName[target]
		if targetID == "" {
			t.Errorf("target node %q missing", target)
			continue
		}
		found := false
		for _, e := range result.Edges {
			if e.Kind == domain.EdgeCalls && string(e.Src) == promoterID && string(e.Tgt) == targetID {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing CALLS edge Promoter.Promote -> %s", target)
		}
	}

	// Cross-package field call must surface as an UnresolvedCall with the
	// right shape (Phase C+D will bind it at promotion time).
	sawAuditWrite := false
	for _, uc := range result.UnresolvedCalls {
		if string(uc.CallerID) == promoterID && uc.PkgQualifier == "ports" && uc.CalleeName == "Write" && uc.IsMethodCall {
			sawAuditWrite = true
			break
		}
	}
	if !sawAuditWrite {
		t.Errorf("missing UnresolvedCall for cross-package interface field p.audit.Write; have: %+v", result.UnresolvedCalls)
	}
}

// TestParseFile_ChainedSelector_UnknownOperandStillFallsThrough guards
// the negative case: a selector whose operand is NOT a tracked local
// variable (e.g. a function parameter, a struct field, an unrecognised
// expression) must keep the prior behaviour — treated as a package
// qualifier with IsMethodCall=false. Otherwise we'd wrongly bind real
// pkg.Foo() calls to method-name lookups.
func TestParseFile_ChainedSelector_UnknownOperandStillFallsThrough(t *testing.T) {
	src := []byte(`package main

import "fmt"

func main(arg string) {
	fmt.Println(arg)
}
`)
	p := treesitter.NewGoParser()
	result, err := p.ParseFile(context.Background(), repoID, filePath, src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, uc := range result.UnresolvedCalls {
		if uc.CalleeName == "Println" && uc.IsMethodCall {
			t.Errorf("fmt.Println must remain a plain pkg call (IsMethodCall=false); got %+v", uc)
		}
	}
}

// TestParseFile_ReceiverSelectorCallsEdge pins solov2-q9p: when a method
// on *Server has body 's.foo()', the parser emits a CALLS edge from
// Server.Bar -> Server.foo. Without this, idiomatic Go (s.x() / s.y())
// produces zero call edges and the call graph is useless.
func TestParseFile_ReceiverSelectorCallsEdge(t *testing.T) {
	src := []byte(`package foo

type Server struct{}

func (s *Server) Foo() {
	s.bar()
}

func (s *Server) bar() {}
`)
	p := treesitter.NewGoParser()
	result, err := p.ParseFile(context.Background(), repoID, filePath, src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	fooNode := findNodeByName(result.Nodes, "Server.Foo")
	barNode := findNodeByName(result.Nodes, "Server.bar")
	if fooNode == nil || barNode == nil {
		t.Fatalf("expected Server.Foo + Server.bar nodes, got: %v", nodeNames(result.Nodes))
		return
	}
	if findEdge(result.Edges, fooNode.ID, barNode.ID, domain.EdgeCalls) == nil {
		t.Errorf("expected CALLS edge Server.Foo -> Server.bar (selector call on receiver); edges=%+v", result.Edges)
	}
}

func TestParseFile_NonGoFile_ReturnsEmpty(t *testing.T) {
	src := []byte(`const x = 1;`)
	p := treesitter.NewGoParser()
	result, err := p.ParseFile(context.Background(), repoID, "pkg/foo/foo.ts", src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Nodes) != 0 || len(result.Edges) != 0 {
		t.Errorf("expected empty result for non-Go file, got %d nodes, %d edges",
			len(result.Nodes), len(result.Edges))
	}
}

func TestParseFile_EmptyFile_ReturnsEmpty(t *testing.T) {
	p := treesitter.NewGoParser()
	result, err := p.ParseFile(context.Background(), repoID, filePath, []byte{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Nodes) != 0 || len(result.Edges) != 0 {
		t.Errorf("expected empty result for empty file, got %d nodes, %d edges",
			len(result.Nodes), len(result.Edges))
	}
}

func TestParseFile_MalformedGo_ReturnsEmptyNoError(t *testing.T) {
	src := []byte(`package foo

func brokenFunc( {
	// missing closing paren / brace
`)
	p := treesitter.NewGoParser()
	result, err := p.ParseFile(context.Background(), repoID, filePath, src)
	if err != nil {
		t.Fatalf("expected nil error for parse-error file, got: %v", err)
	}
	if len(result.Failures) == 0 {
		t.Fatalf("expected at least one ParseFailure for malformed Go source")
	}
}

// TestParseFile_TreeSitterFalsePositive_GoParserAccepts pins solov2-0kv6:
// the tree-sitter Go grammar (smacker fork) lags behind Go's spec — it
// flags valid constructs like `new("string-literal")` (Go 1.26+ new-as-
// converter) as syntax errors. ParseFile must cross-check with go/parser
// and suppress the spurious parse-failure when go/parser accepts the file.
func TestParseFile_TreeSitterFalsePositive_GoParserAccepts(t *testing.T) {
	// Real-world reproducer: `new("h-anchor-old")` is valid Go that the
	// tree-sitter grammar misreads. go/parser accepts it cleanly.
	src := []byte(`package foo

func ptr(s string) *string { return new(s) }

func use() *string {
	return new("h-anchor-old")
}
`)
	p := treesitter.NewGoParser()
	result, err := p.ParseFile(context.Background(), repoID, filePath, src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Failures) != 0 {
		t.Errorf("expected zero failures (go/parser accepts), got %d: %+v",
			len(result.Failures), result.Failures)
	}
	// Siblings must still be extracted — the per-child error-skip should
	// not have dropped `use` either.
	if findNodeByName(result.Nodes, "use") == nil {
		t.Errorf("clean func 'use' was discarded; nodes: %v", nodeNames(result.Nodes))
	}
}

// TestParseFile_RealSyntaxError_StillReported guards the other half of
// solov2-0kv6: when go/parser ALSO rejects the file, ParseFile must keep
// emitting the parse-failure (and prefer go/parser's more precise message).
func TestParseFile_RealSyntaxError_StillReported(t *testing.T) {
	src := []byte(`package foo

func brokenFunc( {
	// unclosed paren / brace
`)
	p := treesitter.NewGoParser()
	result, err := p.ParseFile(context.Background(), repoID, filePath, src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Failures) == 0 {
		t.Fatalf("expected ParseFailure on real syntax error, got none")
	}
}

func TestParseFile_CleanGo_NoFailures(t *testing.T) {
	src := []byte(`package foo

func Ok() {}
`)
	p := treesitter.NewGoParser()
	result, err := p.ParseFile(context.Background(), repoID, filePath, src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Failures) != 0 {
		t.Errorf("expected zero failures on clean Go parse, got %d", len(result.Failures))
	}
}

func TestParseFile_ContainsEdges(t *testing.T) {
	src := []byte(`package foo

func Bar() {}
`)
	p := treesitter.NewGoParser()
	result, err := p.ParseFile(context.Background(), repoID, filePath, src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	pkgNode := findNodeByKind(result.Nodes, domain.KindPackage)
	barNode := findNodeByName(result.Nodes, "Bar")
	if pkgNode == nil {
		t.Fatal("expected a package node")
		return
	}
	if barNode == nil {
		t.Fatal("expected a Bar node")
		return
	}

	edge := findEdge(result.Edges, pkgNode.ID, barNode.ID, domain.EdgeContains)
	if edge == nil {
		t.Errorf("expected CONTAINS edge from package -> Bar, none found")
	}
}

// --- helpers ---

func findNodeByName(nodes []*domain.Node, name string) *domain.Node {
	for _, n := range nodes {
		if n.Name == name {
			return n
		}
	}
	return nil
}

func findNodeByKind(nodes []*domain.Node, kind domain.NodeKind) *domain.Node {
	for _, n := range nodes {
		if n.Kind == kind {
			return n
		}
	}
	return nil
}

func findEdge(edges []*domain.Edge, src, tgt domain.NodeID, kind domain.EdgeKind) *domain.Edge {
	for _, e := range edges {
		if e.Src == src && e.Tgt == tgt && e.Kind == kind {
			return e
		}
	}
	return nil
}

func nodeNames(nodes []*domain.Node) []string {
	names := make([]string, len(nodes))
	for i, n := range nodes {
		names[i] = n.Name
	}
	return names
}
