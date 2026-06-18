// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

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

// TestParseFile_TopLevelVarDecl guards +:
// generic top-level vars stay KindVariable, while a cobra command
// struct-literal is promoted to a KindCommand node named by its Use:
// word (not the Go var identifier). Without either, eng_find_symbol
// returns empty for the CLI surface.
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

	// Generic vars remain KindVariable.
	for _, name := range []string{"verbose", "logFile"} {
		n := findNodeByName(result.Nodes, name)
		if n == nil {
			t.Errorf("expected var %q to be extracted; got none", name)
			continue
		}
		if n.Kind != domain.KindVariable {
			t.Errorf("var %q: kind = %q, want %q", name, n.Kind, domain.KindVariable)
		}
	}

	// rootCmd is promoted to a command named by Use: ("tool"),
	// so the Go var name no longer appears as a KindVariable node.
	if n := findNodeByName(result.Nodes, "rootCmd"); n != nil {
		t.Errorf("rootCmd should be promoted to KindCommand, not emitted as %q", n.Kind)
	}
	cmd := findNodeByName(result.Nodes, "tool")
	if cmd == nil {
		t.Fatal("expected a KindCommand node named 'tool' (from Use:); got none")
	}
	if cmd.Kind != domain.KindCommand {
		t.Errorf("tool: kind = %q, want %q", cmd.Kind, domain.KindCommand)
	}
	// Raw content must include the cobra struct body so semantic search
	// indexes the Use:/Short: strings.
	if cmd.RawContent == nil {
		t.Errorf("command RawContent must be populated for semantic-search visibility")
	}
}

// TestParseFile_CobraCommandTree guards: AddCommand(.)
// wire-up becomes parent→child CONTAINS edges between command nodes, and
// the Use: word - including the "verb [args]" form - names each command.
func TestParseFile_CobraCommandTree(t *testing.T) {
	src := []byte(`package main

import "github.com/spf13/cobra"

var rootCmd = &cobra.Command{Use: "tool"}

var getCmd = &cobra.Command{Use: "get [id]"}

func init() {
	rootCmd.AddCommand(getCmd)
}
`)
	p := treesitter.NewGoParser()
	result, err := p.ParseFile(context.Background(), repoID, filePath, src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	root := findNodeByName(result.Nodes, "tool")
	get := findNodeByName(result.Nodes, "get") // first word of "get [id]"
	if root == nil || get == nil {
		t.Fatalf("expected command nodes 'tool' and 'get'; got root=%v get=%v", root, get)
	}
	if get.Kind != domain.KindCommand {
		t.Errorf("get: kind = %q, want %q", get.Kind, domain.KindCommand)
	}

	found := false
	for _, e := range result.Edges {
		if e.Kind == domain.EdgeContains && e.Src == root.ID && e.Tgt == get.ID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected CONTAINS edge tool→get from rootCmd.AddCommand(getCmd)")
	}
}

// TestParseFile_GinRoute guards: a gin router.METHOD(path,
// handler) call becomes a KindRoute node named "METHOD /path" plus a
// ROUTES route→handler UnresolvedCall (resolved at promotion).
func TestParseFile_GinRoute(t *testing.T) {
	src := []byte(`package main

import "github.com/gin-gonic/gin"

func register(r *gin.Engine) {
	r.GET("/users", listUsers)
}

func listUsers(c *gin.Context) {}
`)
	p := treesitter.NewGoParser()
	result, err := p.ParseFile(context.Background(), repoID, filePath, src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	route := findNodeByName(result.Nodes, "GET /users")
	if route == nil {
		t.Fatalf("expected a KindRoute node named 'GET /users'; got nodes: %v", nodeNames(result.Nodes))
	}
	if route.Kind != domain.KindRoute {
		t.Errorf("route: kind = %q, want %q", route.Kind, domain.KindRoute)
	}

	if !hasRouteHandlerRef(result.UnresolvedCalls, route.ID, "listUsers") {
		t.Errorf("expected ROUTES route→handler ref %q→listUsers; got %+v", route.ID, result.UnresolvedCalls)
	}
}

// TestParseFile_ChiRouteTitleCaseVerb guards: chi spells the
// verb title-case (Get); the route name still normalises to "GET /path"
// and the handler selector resolves through the package qualifier.
func TestParseFile_ChiRouteTitleCaseVerb(t *testing.T) {
	src := []byte(`package main

import "github.com/go-chi/chi/v5"

func register(r chi.Router) {
	r.Get("/items", handlers.List)
}
`)
	p := treesitter.NewGoParser()
	result, err := p.ParseFile(context.Background(), repoID, filePath, src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	route := findNodeByName(result.Nodes, "GET /items")
	if route == nil {
		t.Fatalf("expected KindRoute 'GET /items'; got nodes: %v", nodeNames(result.Nodes))
	}
	if !hasRouteHandlerRef(result.UnresolvedCalls, route.ID, "List") {
		t.Errorf("expected ROUTES ref to handler 'List'; got %+v", result.UnresolvedCalls)
	}
}

// TestParseFile_RoutePrecisionGate guards: selector calls that
// look like routes but fail the gate must NOT promote to KindRoute nodes.
// The title-case case is the key one - chi's verbs (Get/Post) collide with
// common method names, so a gin/echo-only file's client.Post(.) must stay
// inert (only chi enables title-case verbs).
func TestParseFile_RoutePrecisionGate(t *testing.T) {
	cases := map[string][]byte{
		// No gin/echo/chi import: someVar.GET("/x", h) must not misfire.
		"no-router-import": []byte(`package main

func register(x T) {
	x.GET("/x", h)
}
`),
		// Router imported but the path is a variable, not a string literal.
		"dynamic-path": []byte(`package main

import "github.com/gin-gonic/gin"

func register(r *gin.Engine, p string) {
	r.GET(p, h)
}
`),
		// gin imported (upper-case verbs only): a title-case client.Post must
		// not be mistaken for a chi route.
		"gin-file-titlecase-call": []byte(`package main

import "github.com/gin-gonic/gin"

func send(client C, body B) {
	client.Post("https://api/x", body)
}

var _ = gin.New
`),
	}
	p := treesitter.NewGoParser()
	for name, src := range cases {
		result, err := p.ParseFile(context.Background(), repoID, filePath, src)
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", name, err)
		}
		for _, n := range result.Nodes {
			if n.Kind == domain.KindRoute {
				t.Errorf("%s: unexpected KindRoute node %q", name, n.Name)
			}
		}
	}
}

// TestParseFile_EchoRouteFuncLiteralHandler guards: echo uses
// upper-case verbs (like gin); an inline func-literal handler still yields
// a route NODE but no route→handler edge (mirrors the deferred urfave
// Action-closure case).
func TestParseFile_EchoRouteFuncLiteralHandler(t *testing.T) {
	src := []byte(`package main

import "github.com/labstack/echo/v4"

func register(e *echo.Echo) {
	e.POST("/login", func(c echo.Context) error { return nil })
}
`)
	p := treesitter.NewGoParser()
	result, err := p.ParseFile(context.Background(), repoID, filePath, src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	route := findNodeByName(result.Nodes, "POST /login")
	if route == nil {
		t.Fatalf("expected KindRoute 'POST /login'; got nodes: %v", nodeNames(result.Nodes))
	}
	for _, uc := range result.UnresolvedCalls {
		if uc.CallerID == route.ID && uc.EdgeKind == domain.EdgeRoutes {
			t.Errorf("func-literal handler must not emit a route→handler ref, got %+v", uc)
		}
	}
}

// hasRouteHandlerRef reports whether calls contains a ROUTES route→handler
// reference from routeID to a handler with the given callee name.
func hasRouteHandlerRef(calls []domain.UnresolvedCall, routeID domain.NodeID, callee string) bool {
	for _, uc := range calls {
		if uc.CallerID == routeID && uc.CalleeName == callee && uc.EdgeKind == domain.EdgeRoutes {
			return true
		}
	}
	return false
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

// TestParseFile_CallsEdgeCarriesSourceLine guards: every
// CALLS edge must record the 1-indexed line of the call_expression on
// edge.SourceLine. Without this, renderers fall back to the caller
// node's declaration line and a 30-line function with three calls
// reports all three at the same line - exactly the junior-journey
// surprise on the cobra fixture.
func TestParseFile_CallsEdgeCarriesSourceLine(t *testing.T) {
	src := []byte(`package foo

func caller() string {
	leadingPad()
	mid()
	trailing()
	return ""
}

func leadingPad() {}
func mid()        {}
func trailing()   {}
`)
	p := treesitter.NewGoParser()
	result, err := p.ParseFile(context.Background(), repoID, filePath, src)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	caller := findNodeByName(result.Nodes, "caller")
	if caller == nil {
		t.Fatalf("caller node missing")
	}
	want := map[string]int{
		"leadingPad": 4,
		"mid":        5,
		"trailing":   6,
	}
	for name, line := range want {
		callee := findNodeByName(result.Nodes, name)
		if callee == nil {
			t.Errorf("%s node missing", name)
			continue
		}
		edge := findEdge(result.Edges, caller.ID, callee.ID, domain.EdgeCalls)
		if edge == nil {
			t.Errorf("missing CALLS edge caller->%s", name)
			continue
		}
		if edge.SourceLine == nil {
			t.Errorf("CALLS edge caller->%s has nil SourceLine; want %d", name, line)
			continue
		}
		if *edge.SourceLine != line {
			t.Errorf("CALLS edge caller->%s SourceLine = %d, want %d", name, *edge.SourceLine, line)
		}
	}
}

// TestParseFile_ErrorRecovery pins: a syntax error in one
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

// TestParseFile_ImportsAndQualifiedCalls pins: the parser must
// surface the file's import map and capture package-qualified calls
// (cmd.Execute) as UnresolvedCalls carrying a PkgQualifier - the foundation
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

// TestParseFile_ChainedSelectorMethodCall covers:
// a local variable assigned from a package-qualified constructor whose
// subsequent method calls were previously dropped by the parser
// (`g:= greetlib.New(.); g.Hello(.)` only produced Run→New, not
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

// TestParseFile_StructFieldMethodCall covers v1:
// `s.field.Method` where the field is declared with a same-package
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
	// UnresolvedCalls - same-file/same-package resolves directly).
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

// TestParseFile_InterfaceMethodsAsNodes covers v2:
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

// TestParseFile_SamePackageInterfaceFieldCall covers
// v2 same-package path: `p.store.Promote` where store is a field of an
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

// TestParseFile_CrossPackageInterfaceFieldCall covers v2
// cross-package path: `p.audit.Write` where audit is an interface-typed
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

// test mirroring the original epic acceptance: a hexagonal
// Promoter with multiple chained-selector calls through struct fields.
// Each p.X.M chain must produce an UnresolvedCall or an in-file edge.
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

// TestParseFile_AnonCallsInTopLevelVar covers (anonymous
// functions in top-level var initialisers contribute CALLS edges)
// extended by (attribution is the SURROUNDING VAR, not the
// package node, whenever the var has a resolvable name). Legacy
// behaviour attributed both calls to the package node - that hid the
// caller's identity for every cobra-app initialiser. New behaviour:
// `root = func{ serveRoot }` produces root → serveRoot, and
// `chk = func{ validate }` produces chk → validate. The package
// node is no longer the CALLS src for these.
func TestParseFile_AnonCallsInTopLevelVar(t *testing.T) {
	src := []byte(`package cli

func serveRoot() {}
func validate() bool { return true }

var (
	root = func() { serveRoot() }
	chk  = func() bool { return validate() }
)
`)
	p := treesitter.NewGoParser()
	result, err := p.ParseFile(context.Background(), repoID, filePath, src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var pkgID, rootID, chkID, serveRootID, validateID string
	for _, n := range result.Nodes {
		switch n.Name {
		case "cli":
			pkgID = string(n.ID)
		case "root":
			rootID = string(n.ID)
		case "chk":
			chkID = string(n.ID)
		case "serveRoot":
			serveRootID = string(n.ID)
		case "validate":
			validateID = string(n.ID)
		}
	}
	if pkgID == "" || rootID == "" || chkID == "" || serveRootID == "" || validateID == "" {
		t.Fatalf("expected package + var + function nodes, got %+v", result.Nodes)
	}

	hasEdge := func(src, tgt string) bool {
		for _, e := range result.Edges {
			if e.Kind == domain.EdgeCalls && string(e.Src) == src && string(e.Tgt) == tgt {
				return true
			}
		}
		return false
	}
	if !hasEdge(rootID, serveRootID) {
		t.Errorf("missing CALLS edge root -> serveRoot (from var's func literal)")
	}
	if !hasEdge(chkID, validateID) {
		t.Errorf("missing CALLS edge chk -> validate (from var's func literal)")
	}
	// Negative: must NOT attribute to package regression.
	if hasEdge(pkgID, serveRootID) || hasEdge(pkgID, validateID) {
		t.Errorf("anon-func calls must attribute to surrounding var, not package node")
	}
}

// TestParseFile_ChainedSelector_UnknownOperandStillFallsThrough guards
// the negative case: a selector whose operand is NOT a tracked local
// variable (e.g. a function parameter, a struct field, an unrecognised
// expression) must keep the prior behaviour - treated as a package
// qualifier with IsMethodCall=false. Otherwise we'd wrongly bind real
// pkg.Foo calls to method-name lookups.
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

// TestParseFile_ReceiverSelectorCallsEdge pins: when a method
// on *Server has body 's.foo', the parser emits a CALLS edge from
// Server.Bar -> Server.foo. Without this, idiomatic Go (s.x / s.y)
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

// TestParseFile_TreeSitterFalsePositive_GoParserAccepts pins:
// the tree-sitter Go grammar (smacker fork) lags behind Go's spec - it
// flags valid constructs like `new("string-literal")` (Go 1.26+ new-as
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
	// Siblings must still be extracted - the per-child error-skip should
	// not have dropped `use` either.
	if findNodeByName(result.Nodes, "use") == nil {
		t.Errorf("clean func 'use' was discarded; nodes: %v", nodeNames(result.Nodes))
	}
}

// TestParseFile_RealSyntaxError_StillReported guards the other half of
// when go/parser ALSO rejects the file, ParseFile must keep
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

// helpers

func findNodeByName(nodes []*domain.Node, name string) *domain.Node {
	for _, n := range nodes {
		if n.Name == name {
			return n
		}
	}
	return nil
}

// hasContainsEdge reports whether a CONTAINS edge src→tgt is present.
func hasContainsEdge(edges []*domain.Edge, src, tgt domain.NodeID) bool {
	for _, e := range edges {
		if e.Kind == domain.EdgeContains && e.Src == src && e.Tgt == tgt {
			return true
		}
	}
	return false
}

// findCommand returns the KindCommand node with the given name, or nil - a
// kind-aware finder so a struct type (StopCmd) doesn't shadow its command.
func findCommand(nodes []*domain.Node, name string) *domain.Node {
	for _, n := range nodes {
		if n.Name == name && n.Kind == domain.KindCommand {
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

// TestParseFile_FunctionLocalTypesIgnored pins: Go allows
// declaring named types inside function bodies, and real codebases (hugo:
// common/hreflect/helpers_test.go) routinely declare the same local name
// (`type k string`) inside two different functions. Those are not part of
// the symbol graph; the parser must skip them so they don't collide on
// node_id and break the promotion tx with a UNIQUE-PK on nodes (1555).
func TestParseFile_FunctionLocalTypesIgnored(t *testing.T) {
	src := []byte(`package foo

func a() {
	type k string
	_ = k("")
}

func b() {
	type k string
	_ = k("")
}
`)
	p := treesitter.NewGoParser()
	result, err := p.ParseFile(context.Background(), repoID, filePath, src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, n := range result.Nodes {
		if n.Name == "k" {
			t.Errorf("expected no node for function-local type 'k', got %#v", n)
		}
	}
}

// TestParseFile_MultipleInitFunctions pins: Go allows multiple
// `func init` per file (protobuf-generated.pb.go files routinely have
// two). Each must produce a distinct node_id; otherwise the promotion tx
// fails on the UNIQUE-PK constraint and cold-scan crashes mid-promote.
func TestParseFile_MultipleInitFunctions(t *testing.T) {
	src := []byte(`package foo

func init() {
	_ = 1
}

func init() {
	_ = 2
}

func init() {
	_ = 3
}
`)
	p := treesitter.NewGoParser()
	result, err := p.ParseFile(context.Background(), repoID, filePath, src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	inits := make([]*domain.Node, 0, 3)
	for _, n := range result.Nodes {
		if n.Name == "init" {
			inits = append(inits, n)
		}
	}
	if len(inits) != 3 {
		t.Fatalf("expected 3 init() nodes, got %d", len(inits))
	}
	seen := map[domain.NodeID]int{}
	for _, n := range inits {
		seen[n.ID]++
	}
	if len(seen) != 3 {
		t.Errorf("expected 3 distinct node IDs, got %d (duplicates: %v)", len(seen), seen)
	}
}

func nodeNames(nodes []*domain.Node) []string {
	names := make([]string, len(nodes))
	for i, n := range nodes {
		names[i] = n.Name
	}
	return names
}

// TestParseFile_FunctionPassedAsArgumentEmitsCallsEdge covers the
// pflag-shaped half of: a same-file function passed by
// value to another call (`f.getFlagType(name, "bool", boolConv)`)
// must produce a CALLS edge to that function so the dead-code rule
// doesn't flag it. Before this fix the parser only emitted CALLS for
// direct invocation (boolConv(.)), so every *Conv helper in pflag
// appeared dead.
func TestParseFile_FunctionPassedAsArgumentEmitsCallsEdge(t *testing.T) {
	src := []byte(`package pflag

func boolConv(s string) bool { return s == "true" }

func getFlagType(name string, conv func(string) bool) {}

func DoIt() {
	getFlagType("bool", boolConv)
}
`)
	p := treesitter.NewGoParser()
	result, err := p.ParseFile(context.Background(), repoID, filePath, src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var doItID, boolConvID domain.NodeID
	for _, n := range result.Nodes {
		switch n.Name {
		case "DoIt":
			doItID = n.ID
		case "boolConv":
			boolConvID = n.ID
		}
	}
	if doItID == "" || boolConvID == "" {
		t.Fatalf("missing nodes; got %d", len(result.Nodes))
	}
	for _, e := range result.Edges {
		if e.Src == doItID && e.Tgt == boolConvID && e.Kind == "CALLS" {
			return
		}
	}
	t.Errorf("missing CALLS edge DoIt → boolConv (function-value passing). Edges: %d, Unresolved: %+v", len(result.Edges), result.UnresolvedCalls)
}

// TestParseFile_BareIdentifierArg_NonFunctionSkipped guards the
// negative: a bare identifier argument that is NOT a same-file
// function/method (e.g. a parameter, a local variable, a constant)
// must NOT produce a CALLS edge to a phantom node. 's
// argument-passing rule is symbol-map-gated for exactly this reason.
func TestParseFile_BareIdentifierArg_NonFunctionSkipped(t *testing.T) {
	src := []byte(`package x

func helper(s string) {}

func DoIt(name string) {
	helper(name)
}
`)
	p := treesitter.NewGoParser()
	result, err := p.ParseFile(context.Background(), repoID, filePath, src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Should produce DoIt→helper (direct call) and NO edge whose
	// callee resolves through 'name'. Unresolved calls also should
	// not contain 'name' as a callee.
	for _, uc := range result.UnresolvedCalls {
		if uc.CalleeName == "name" {
			t.Errorf("'name' parameter passed as arg must not become an UnresolvedCall: %+v", uc)
		}
	}
}

// TestParseFile_AnonFuncInVarInitAttributesToSurroundingVar pins
// calls inside an anonymous function nested in a
// top-level var initialiser (the dominant cobra-app shape:
// `var helloCmd = &cobra.Command{ RunE: func{ Foo } }`) must
// attribute to the surrounding var node (helloCmd), not the package
// node. Before the fix, cross-repo blast on Foo named "package cmd"
// as the caller for every cobra app in the workspace - a known false
// signal documented with the "package-grain src" disclaimer that
// shipped with the prior workaround.
func TestParseFile_AnonFuncInVarInitAttributesToSurroundingVar(t *testing.T) {
	src := []byte(`package cmd

func Foo() {}

var helloCmd = struct {
	RunE func()
}{
	RunE: func() {
		Foo()
	},
}
`)
	p := treesitter.NewGoParser()
	result, err := p.ParseFile(context.Background(), repoID, filePath, src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var helloID, fooID, pkgID domain.NodeID
	for _, n := range result.Nodes {
		switch n.Name {
		case "helloCmd":
			helloID = n.ID
		case "Foo":
			fooID = n.ID
		case "cmd":
			pkgID = n.ID
		}
	}
	if helloID == "" || fooID == "" || pkgID == "" {
		t.Fatalf("missing nodes; got %d", len(result.Nodes))
	}

	var fromHello, fromPkg bool
	for _, e := range result.Edges {
		if e.Tgt != fooID || e.Kind != "CALLS" {
			continue
		}
		switch e.Src {
		case helloID:
			fromHello = true
		case pkgID:
			fromPkg = true
		}
	}
	if !fromHello {
		t.Errorf("expected CALLS edge helloCmd → Foo; edges: %+v", result.Edges)
	}
	if fromPkg {
		t.Errorf("anon-func call must NOT attribute to package node; should attribute to surrounding var helloCmd")
	}
}

// TestParseFile_UrfaveCommandTree guards: a urfave
// `var app = &cli.App{Name:., Commands: *cli.Command{.}}` is
// promoted to a KindCommand named by Name:, each Commands-slice literal
// becomes a child KindCommand named by its own Name:, and an app→child
// CONTAINS edge links them. The versioned import path (/v2) must still
// resolve to urfave.
func TestParseFile_UrfaveCommandTree(t *testing.T) {
	src := []byte(`package main

import "github.com/urfave/cli/v2"

var app = &cli.App{
	Name: "tool",
	Commands: []*cli.Command{
		{Name: "add", Action: addAction},
		{Name: "rm"},
	},
}

func addAction() {}
`)
	p := treesitter.NewGoParser()
	result, err := p.ParseFile(context.Background(), repoID, filePath, src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// The Go var "app" is promoted, so it's no longer a KindVariable.
	if n := findNodeByName(result.Nodes, "app"); n != nil {
		t.Errorf("app should be promoted to KindCommand, not emitted as %q", n.Kind)
	}

	appNode := findNodeByName(result.Nodes, "tool")
	add := findNodeByName(result.Nodes, "add")
	rm := findNodeByName(result.Nodes, "rm")
	if appNode == nil || add == nil || rm == nil {
		t.Fatalf("expected command nodes tool/add/rm; got app=%v add=%v rm=%v", appNode, add, rm)
	}
	for _, n := range []*domain.Node{appNode, add, rm} {
		if n.Kind != domain.KindCommand {
			t.Errorf("%s: kind = %q, want %q", n.Name, n.Kind, domain.KindCommand)
		}
	}

	want := map[domain.NodeID]bool{add.ID: false, rm.ID: false}
	for _, e := range result.Edges {
		if e.Kind == domain.EdgeContains && e.Src == appNode.ID {
			if _, ok := want[e.Tgt]; ok {
				want[e.Tgt] = true
			}
		}
	}
	if !want[add.ID] {
		t.Errorf("expected CONTAINS edge tool→add")
	}
	if !want[rm.ID] {
		t.Errorf("expected CONTAINS edge tool→rm")
	}
}

// TestParseFile_UrfaveByReferenceSubcommands guards 's
// by-reference idiom: subcommands declared as their own top-level
// `var addCmd = &cli.Command{Name:.}` and linked into the App by
// identifier (`Commands: *cli.Command{addCmd}`). Both the App and the
// referenced command must promote to KindCommand, with an app→addCmd
// CONTAINS edge - even though addCmd is declared after the App.
func TestParseFile_UrfaveByReferenceSubcommands(t *testing.T) {
	src := []byte(`package main

import "github.com/urfave/cli/v2"

var app = &cli.App{
	Name:     "tool",
	Commands: []*cli.Command{addCmd},
}

var addCmd = &cli.Command{Name: "add"}
`)
	p := treesitter.NewGoParser()
	result, err := p.ParseFile(context.Background(), repoID, filePath, src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	appNode := findNodeByName(result.Nodes, "tool")
	add := findNodeByName(result.Nodes, "add")
	if appNode == nil || add == nil {
		t.Fatalf("expected command nodes tool/add; got app=%v add=%v", appNode, add)
	}
	if add.Kind != domain.KindCommand {
		t.Errorf("add: kind = %q, want %q", add.Kind, domain.KindCommand)
	}
	// addCmd's Go var name must not survive as a KindVariable.
	if n := findNodeByName(result.Nodes, "addCmd"); n != nil {
		t.Errorf("addCmd should be promoted to KindCommand, not emitted as %q", n.Kind)
	}

	found := false
	for _, e := range result.Edges {
		if e.Kind == domain.EdgeContains && e.Src == appNode.ID && e.Tgt == add.ID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected CONTAINS edge tool→add from by-reference Commands slice")
	}
}

// TestParseFile_CobraRunEAttributesToCommand guards 's
// interaction with the cobra-app grain: when a command var
// is promoted to KindCommand it must STILL be the caller of its own
// RunE-closure calls, not the package node. The promotion removes the
// KindVariable entry the anon-call walker keyed on, so the command node
// is re-registered under its Go var name - this test pins that.
func TestParseFile_CobraRunEAttributesToCommand(t *testing.T) {
	src := []byte(`package cmd

import "github.com/spf13/cobra"

func Foo() {}

var helloCmd = &cobra.Command{
	Use: "hello",
	RunE: func() {
		Foo()
	},
}
`)
	p := treesitter.NewGoParser()
	result, err := p.ParseFile(context.Background(), repoID, filePath, src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var helloID, fooID, pkgID domain.NodeID
	for _, n := range result.Nodes {
		switch {
		case n.Name == "hello" && n.Kind == domain.KindCommand:
			helloID = n.ID
		case n.Name == "Foo":
			fooID = n.ID
		case n.Name == "cmd" && n.Kind == domain.KindPackage:
			pkgID = n.ID
		}
	}
	if helloID == "" || fooID == "" || pkgID == "" {
		t.Fatalf("missing nodes; got %d", len(result.Nodes))
	}

	var fromHello, fromPkg bool
	for _, e := range result.Edges {
		if e.Tgt != fooID || e.Kind != domain.EdgeCalls {
			continue
		}
		switch e.Src {
		case helloID:
			fromHello = true
		case pkgID:
			fromPkg = true
		}
	}
	if !fromHello {
		t.Errorf("expected CALLS edge hello-command → Foo; edges: %+v", result.Edges)
	}
	if fromPkg {
		t.Errorf("RunE-closure call must attribute to the command, not the package node")
	}
}

// TestParseFile_KongStructTagCommands guards: kong models
// commands as struct fields with `cmd:""` tags, not composite literals.
// Each tagged field becomes a KindCommand named by the dasherized field
// name (or a `name:` override); nesting follows the field type, so the root
// CLI struct's fields are top-level commands and a command struct's own cmd
// fields are its subcommands (CONTAINS). Flag/arg fields are not commands.
// kongCLISrc is the fixture for TestParseFile_KongStructTagCommands, hoisted
// out of the test body so the struct-tag backticks don't bloat it past the
// funlen budget. CLI's cmd fields are top-level commands; ServerCmd nests
// Start/Stop; Rm overrides its name; Verbose/Addr are flag/arg fields.
const kongCLISrc = "package main\n\n" +
	"import \"github.com/alecthomas/kong\"\n\n" +
	"type CLI struct {\n" +
	"\tVerbose bool      `help:\"Be loud.\"`\n" +
	"\tServer  ServerCmd `cmd:\"\" help:\"Server ops.\"`\n" +
	"\tRm      RmCmd      `cmd:\"\" name:\"remove\"`\n" +
	"}\n\n" +
	"type ServerCmd struct {\n" +
	"\tStart  StartCmd  `cmd:\"\"`\n" +
	"\tStop   StopCmd   `cmd:\"\"`\n" +
	"\tDryRun DryRunCmd `cmd:\"\"`\n" +
	"}\n\n" +
	"type DryRunCmd struct{}\n\n" +
	"type StartCmd struct {\n\tAddr string `arg:\"\"`\n}\n\n" +
	"type StopCmd struct{}\n\n" +
	"type RmCmd struct{}\n\n" +
	"var cli CLI\n\n" +
	"func main() {\n\tkong.Parse(&cli)\n}\n"

func TestParseFile_KongStructTagCommands(t *testing.T) {
	p := treesitter.NewGoParser()
	result, err := p.ParseFile(context.Background(), repoID, filePath, []byte(kongCLISrc))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	server := findCommand(result.Nodes, "server")
	remove := findCommand(result.Nodes, "remove") // name: override
	start := findCommand(result.Nodes, "start")
	stop := findCommand(result.Nodes, "stop")
	if server == nil || remove == nil || start == nil || stop == nil {
		t.Fatalf("expected commands server/remove/start/stop; got %v", nodeNames(result.Nodes))
	}
	// Flag/arg fields must not be promoted to commands.
	if n := findCommand(result.Nodes, "verbose"); n != nil {
		t.Errorf("flag field Verbose must not become a command")
	}
	if n := findCommand(result.Nodes, "addr"); n != nil {
		t.Errorf("arg field Addr must not become a command")
	}

	if !hasContainsEdge(result.Edges, server.ID, start.ID) {
		t.Errorf("expected CONTAINS server→start")
	}
	if !hasContainsEdge(result.Edges, server.ID, stop.ID) {
		t.Errorf("expected CONTAINS server→stop")
	}
	// Multi-word field name kebab-cases (DryRun → dry-run), like kong.
	dryRun := findCommand(result.Nodes, "dry-run")
	if dryRun == nil || !hasContainsEdge(result.Edges, server.ID, dryRun.ID) {
		t.Errorf("expected command 'dry-run' contained by server; got %v", nodeNames(result.Nodes))
	}
	// Top-level commands (CLI's fields) must have no parent COMMAND - the
	// package node still CONTAINS them, but no other command does.
	cmds := []*domain.Node{server, remove, start, stop}
	for _, parent := range cmds {
		if hasContainsEdge(result.Edges, parent.ID, server.ID) || hasContainsEdge(result.Edges, parent.ID, remove.ID) {
			t.Errorf("top-level command must not be contained by command %q", parent.Name)
		}
	}
}

// TestParseFile_SamePackageConstructorMethodCall covers:
// when a test (or any same-package caller) does `g:= New(.)` followed
// by `g.Render`, the Render call must bind to Greeting.Render in the
// same file. Before this fix the parser emitted UnresolvedCall with
// PkgQualifier="g" - a bare local-var name that couldn't possibly
// resolve at promotion. Junior-journey symptom: `veska blast Render`
// on greetlib showed zero in-repo callers even though greet_test.go
// literally calls g.Render twice.
func TestParseFile_SamePackageConstructorMethodCall(t *testing.T) {
	src := []byte(`package greetlib

type Greeting struct{ Name string }

func New(name string) Greeting {
	return Greeting{Name: name}
}

func (g Greeting) Render() string { return g.Name }

func TestRender() {
	g := New("world")
	_ = g.Render()
}
`)
	p := treesitter.NewGoParser()
	result, err := p.ParseFile(context.Background(), repoID, filePath, src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// The TestRender → Greeting.Render edge must materialise as a
	// resolved CALLS edge (both endpoints live in this file), not an
	// UnresolvedCall - there's no cross-package indirection here.
	var hasEdge bool
	var renderNodeID, testRenderNodeID domain.NodeID
	for _, n := range result.Nodes {
		switch n.Name {
		case "Greeting.Render":
			renderNodeID = n.ID
		case "TestRender":
			testRenderNodeID = n.ID
		}
	}
	if renderNodeID == "" || testRenderNodeID == "" {
		t.Fatalf("missing expected nodes; got %d nodes", len(result.Nodes))
	}
	for _, e := range result.Edges {
		if e.Src == testRenderNodeID && e.Tgt == renderNodeID && e.Kind == "CALLS" {
			hasEdge = true
		}
	}
	if !hasEdge {
		t.Errorf("missing CALLS edge TestRender → Greeting.Render. Unresolved instead: %+v", result.UnresolvedCalls)
	}
}

// TestParseFile_SamePackageConstructorMethodCall_PointerReturn covers
// the pointer-return shape of: `func New *Greeting`
// followed by `g:= New; g.Method`. simpleReturnTypeName must
// unwrap *T to T so the in-file lookup binds the same way as the
// value-return case.
func TestParseFile_SamePackageConstructorMethodCall_PointerReturn(t *testing.T) {
	src := []byte(`package greetlib

type Greeting struct{ Name string }

func NewPtr(name string) *Greeting {
	return &Greeting{Name: name}
}

func (g *Greeting) Render() string { return g.Name }

func TestRenderPtr() {
	g := NewPtr("world")
	_ = g.Render()
}
`)
	p := treesitter.NewGoParser()
	result, err := p.ParseFile(context.Background(), repoID, filePath, src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var renderNodeID, callerNodeID domain.NodeID
	for _, n := range result.Nodes {
		switch n.Name {
		case "Greeting.Render":
			renderNodeID = n.ID
		case "TestRenderPtr":
			callerNodeID = n.ID
		}
	}
	if renderNodeID == "" || callerNodeID == "" {
		t.Fatalf("missing nodes; got %d", len(result.Nodes))
	}
	for _, e := range result.Edges {
		if e.Src == callerNodeID && e.Tgt == renderNodeID && e.Kind == "CALLS" {
			return
		}
	}
	t.Errorf("missing TestRenderPtr → Greeting.Render edge. Unresolved: %+v", result.UnresolvedCalls)
}

// TestParseFile_PackageVarCompositeLiteralOrigin covers the cobra-shaped
// pattern surfaced in the junior onboarding journey ( /
// ): a package-level var initialised from a composite literal
// `&pkg.Type{.}` is the dominant cobra app shape, and subsequent
// `rootCmd.AddCommand(.)` calls must emit method-call UnresolvedCalls
// against the pkg's import path. Before this fix collectLocalVarOrigins
// only walked function bodies and only recognised `v:= pkg.F(.)`, so
// package-scoped vars holding a composite literal became
// PkgQualifier="rootCmd" - an unresolvable bareword that never produced
// a cross-repo stub.
func TestParseFile_PackageVarCompositeLiteralOrigin(t *testing.T) {
	src := []byte(`package cmd

import "github.com/spf13/cobra"

var rootCmd = &cobra.Command{Use: "app"}
var helloCmd = &cobra.Command{Use: "hello"}

func init() {
	rootCmd.AddCommand(helloCmd)
}
`)
	p := treesitter.NewGoParser()
	result, err := p.ParseFile(context.Background(), repoID, filePath, src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var sawAddCommand, addCommandIsMethod bool
	for _, uc := range result.UnresolvedCalls {
		if uc.CalleeName == "AddCommand" && uc.PkgQualifier == "cobra" {
			sawAddCommand = true
			addCommandIsMethod = uc.IsMethodCall
		}
	}
	if !sawAddCommand {
		t.Fatalf("UnresolvedCalls missing rootCmd.AddCommand resolved to cobra.AddCommand; got %+v", result.UnresolvedCalls)
	}
	if !addCommandIsMethod {
		t.Errorf("rootCmd.AddCommand must be flagged IsMethodCall=true so the resolver suffix-matches Command.AddCommand in cobra")
	}
}

// TestParseFile_PackageVarConstructorOrigin guards the package-scope
// variant of the existing function-scope rule: `var x = pkg.F(.)` at
// file scope should be treated the same as `x:= pkg.F(.)` inside a
// function body - its method calls must classify as method-call
// UnresolvedCalls against pkg's import path.
func TestParseFile_PackageVarConstructorOrigin(t *testing.T) {
	src := []byte(`package app

import "github.com/jrose/greetlib"

var g = greetlib.New("hi")

func Run() string {
	return g.Render()
}
`)
	p := treesitter.NewGoParser()
	result, err := p.ParseFile(context.Background(), repoID, filePath, src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var sawRender, renderIsMethod bool
	for _, uc := range result.UnresolvedCalls {
		if uc.PkgQualifier == "greetlib" && uc.CalleeName == "Render" {
			sawRender = true
			renderIsMethod = uc.IsMethodCall
		}
	}
	if !sawRender {
		t.Fatalf("UnresolvedCalls missing g.Render → greetlib.Render: %+v", result.UnresolvedCalls)
	}
	if !renderIsMethod {
		t.Errorf("g.Render must be flagged IsMethodCall=true; got false")
	}
}
