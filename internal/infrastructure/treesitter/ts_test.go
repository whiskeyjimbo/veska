package treesitter_test

import (
	"context"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/treesitter"
)

const tsRepoID = "test-repo-ts"

func TestTS_FunctionDeclaration(t *testing.T) {
	src := []byte(`
function greet(name: string): string {
  return "hello " + name;
}
`)
	p := treesitter.NewTSParser()
	result, err := p.ParseFile(context.Background(), tsRepoID, "src/utils.ts", src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	fn := findNodeByName(result.Nodes, "greet")
	if fn == nil {
		t.Fatalf("expected node named 'greet', got nodes: %v", nodeNames(result.Nodes))
		return
	}
	if fn.Kind != domain.KindFunction {
		t.Errorf("expected KindFunction, got %q", fn.Kind)
	}
	if fn.Language == nil || *fn.Language != "typescript" {
		t.Errorf("expected language 'typescript', got %v", fn.Language)
	}
}

func TestTS_ClassDeclaration(t *testing.T) {
	src := []byte(`
class Animal {
  name: string;
  constructor(name: string) { this.name = name; }
}
`)
	p := treesitter.NewTSParser()
	result, err := p.ParseFile(context.Background(), tsRepoID, "src/animal.ts", src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	cls := findNodeByName(result.Nodes, "Animal")
	if cls == nil {
		t.Fatalf("expected node named 'Animal', got nodes: %v", nodeNames(result.Nodes))
		return
	}
	if cls.Kind != domain.KindClass {
		t.Errorf("expected KindClass, got %q", cls.Kind)
	}
}

func TestTS_MethodInClass(t *testing.T) {
	src := []byte(`
class Dog {
  bark(): string {
    return "woof";
  }
}
`)
	p := treesitter.NewTSParser()
	result, err := p.ParseFile(context.Background(), tsRepoID, "src/dog.ts", src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	method := findNodeByName(result.Nodes, "Dog.bark")
	if method == nil {
		t.Fatalf("expected node named 'Dog.bark', got nodes: %v", nodeNames(result.Nodes))
		return
	}
	if method.Kind != domain.KindMethod {
		t.Errorf("expected KindMethod, got %q", method.Kind)
	}
}

func TestTS_InterfaceDeclaration(t *testing.T) {
	src := []byte(`
interface Shape {
  area(): number;
}
`)
	p := treesitter.NewTSParser()
	result, err := p.ParseFile(context.Background(), tsRepoID, "src/shape.ts", src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	iface := findNodeByName(result.Nodes, "Shape")
	if iface == nil {
		t.Fatalf("expected node named 'Shape', got nodes: %v", nodeNames(result.Nodes))
		return
	}
	if iface.Kind != domain.KindInterface {
		t.Errorf("expected KindInterface, got %q", iface.Kind)
	}
}

func TestTSX_JSXParsesWithoutError(t *testing.T) {
	src := []byte(`
import React from 'react';

function Greeting({ name }: { name: string }) {
  return <div className="greeting">Hello {name}</div>;
}

export default Greeting;
`)
	p := treesitter.NewTSParser()
	result, err := p.ParseFile(context.Background(), tsRepoID, "src/GreetingComponent.tsx", src)
	if err != nil {
		t.Fatalf("TSX with JSX should not error, got: %v", err)
	}

	fn := findNodeByName(result.Nodes, "Greeting")
	if fn == nil {
		t.Fatalf("expected node named 'Greeting', got nodes: %v", nodeNames(result.Nodes))
		return
	}
	if fn.Kind != domain.KindFunction {
		t.Errorf("expected KindFunction, got %q", fn.Kind)
	}
}

func TestTS_RoutingTsVsTsx(t *testing.T) {
	src := []byte(`
function hello(): string {
  return "hi";
}
`)
	p := treesitter.NewTSParser()

	// .ts
	rTS, err := p.ParseFile(context.Background(), tsRepoID, "src/hello.ts", src)
	if err != nil {
		t.Fatalf(".ts parse error: %v", err)
	}
	if findNodeByName(rTS.Nodes, "hello") == nil {
		t.Error("expected 'hello' node for .ts file")
	}

	// .tsx
	rTSX, err := p.ParseFile(context.Background(), tsRepoID, "src/hello.tsx", src)
	if err != nil {
		t.Fatalf(".tsx parse error: %v", err)
	}
	if findNodeByName(rTSX.Nodes, "hello") == nil {
		t.Error("expected 'hello' node for .tsx file")
	}
}

func TestTS_NonTSFileReturnsEmpty(t *testing.T) {
	src := []byte(`package main`)
	p := treesitter.NewTSParser()
	result, err := p.ParseFile(context.Background(), tsRepoID, "main.go", src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Nodes) != 0 || len(result.Edges) != 0 {
		t.Errorf("expected empty result, got %d nodes, %d edges", len(result.Nodes), len(result.Edges))
	}
}

func TestTS_CallsEdge(t *testing.T) {
	src := []byte(`
function hello(): string {
  return "hi";
}

function greet(): string {
  return hello();
}
`)
	p := treesitter.NewTSParser()
	result, err := p.ParseFile(context.Background(), tsRepoID, "src/calls.ts", src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	helloNode := findNodeByName(result.Nodes, "hello")
	greetNode := findNodeByName(result.Nodes, "greet")
	if helloNode == nil || greetNode == nil {
		t.Fatalf("expected hello and greet nodes, got: %v", nodeNames(result.Nodes))
		return
	}

	edge := findEdge(result.Edges, greetNode.ID, helloNode.ID, domain.EdgeCalls)
	if edge == nil {
		t.Error("expected CALLS edge from greet -> hello")
	}
}

// TestTS_ThisCallsEdge_IntraClass covers solov2-gv6: a `this.foo()`
// call inside a method on class C must emit a CALLS edge to C.foo,
// matching the Go parser's receiver-selector resolution.
func TestTS_ThisCallsEdge_IntraClass(t *testing.T) {
	src := []byte(`
class Server {
  start(): void {
    this.listen();
  }
  listen(): void {}
}
`)
	p := treesitter.NewTSParser()
	result, err := p.ParseFile(context.Background(), tsRepoID, "src/server.ts", src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	start := findNodeByName(result.Nodes, "Server.start")
	listen := findNodeByName(result.Nodes, "Server.listen")
	if start == nil || listen == nil {
		t.Fatalf("expected Server.start and Server.listen nodes, got: %v", nodeNames(result.Nodes))
	}
	if findEdge(result.Edges, start.ID, listen.ID, domain.EdgeCalls) == nil {
		t.Errorf("expected CALLS edge Server.start -> Server.listen; got edges: %d", len(result.Edges))
	}
}

// TestTS_ThisCallsEdge_FromConstructor covers AC2: the constructor of a
// class is itself a method whose this.foo() calls must resolve. Without
// this, the most common "wire dependencies" pattern is invisible.
func TestTS_ThisCallsEdge_FromConstructor(t *testing.T) {
	src := []byte(`
class App {
  constructor() {
    this.boot();
  }
  boot(): void {}
}
`)
	p := treesitter.NewTSParser()
	result, err := p.ParseFile(context.Background(), tsRepoID, "src/app.ts", src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	ctor := findNodeByName(result.Nodes, "App.constructor")
	boot := findNodeByName(result.Nodes, "App.boot")
	if ctor == nil || boot == nil {
		t.Fatalf("expected App.constructor and App.boot nodes, got: %v", nodeNames(result.Nodes))
	}
	if findEdge(result.Edges, ctor.ID, boot.ID, domain.EdgeCalls) == nil {
		t.Errorf("expected CALLS edge App.constructor -> App.boot")
	}
}

func TestTS_ParseFailureSurfaced(t *testing.T) {
	// Unclosed brace + bogus token — tree-sitter will mark ERROR nodes.
	src := []byte(`
function broken(
  return
`)
	p := treesitter.NewTSParser()
	result, err := p.ParseFile(context.Background(), tsRepoID, "src/bad.ts", src)
	if err != nil {
		t.Fatalf("ParseFile should not return a hard error for syntax errors, got: %v", err)
	}
	if len(result.Failures) == 0 {
		t.Fatalf("expected at least one ParseFailure for malformed source, got none")
	}
	if result.Failures[0].Message == "" {
		t.Error("expected non-empty failure message")
	}
}

func TestTS_CleanParseHasNoFailures(t *testing.T) {
	src := []byte(`
function ok(): string { return "ok"; }
`)
	p := treesitter.NewTSParser()
	result, err := p.ParseFile(context.Background(), tsRepoID, "src/ok.ts", src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Failures) != 0 {
		t.Errorf("expected zero failures on clean parse, got %d", len(result.Failures))
	}
}

func TestTS_ModuleNode(t *testing.T) {
	src := []byte(`
function foo(): void {}
`)
	p := treesitter.NewTSParser()
	result, err := p.ParseFile(context.Background(), tsRepoID, "src/utils.ts", src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	mod := findNodeByKind(result.Nodes, domain.KindModule)
	if mod == nil {
		t.Fatal("expected a module node")
		return
	}
	if mod.Name != "utils" {
		t.Errorf("expected module name 'utils', got %q", mod.Name)
	}

	fooNode := findNodeByName(result.Nodes, "foo")
	if fooNode == nil {
		t.Fatal("expected foo node")
		return
	}

	edge := findEdge(result.Edges, mod.ID, fooNode.ID, domain.EdgeContains)
	if edge == nil {
		t.Error("expected CONTAINS edge from module -> foo")
	}
}
