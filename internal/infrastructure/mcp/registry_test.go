package mcp

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

// noopHandler is a minimal ToolHandler for use in tests.
func noopHandler(_ context.Context, _ domain.Actor, _ json.RawMessage) (any, *RPCError) {
	return nil, nil
}

func makeSpec(name, desc string) ToolSpec {
	return ToolSpec{
		Name:        name,
		Description: desc,
		Handler:     noopHandler,
	}
}

// ---------------------------------------------------------------------------
// Register — happy path
// ---------------------------------------------------------------------------

func TestRegister_ValidSpec(t *testing.T) {
	r := NewRegistry()
	err := r.Register(makeSpec("eng_find_symbol", "finds a symbol by name in the graph"))
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Register — name validation
// ---------------------------------------------------------------------------

func TestRegister_NoPrefixRejectsName(t *testing.T) {
	r := NewRegistry()
	err := r.Register(makeSpec("find_symbol", "finds a symbol by name in the graph"))
	if err == nil {
		t.Fatal("expected error for missing eng_ prefix, got nil")
		return
	}
}

func TestRegister_UnknownVerbRejectsName(t *testing.T) {
	r := NewRegistry()
	err := r.Register(makeSpec("eng_delete_node", "deletes a node from the graph forcibly"))
	if err == nil {
		t.Fatal("expected error for unknown verb 'delete', got nil")
		return
	}
}

func TestRegister_AllValidVerbsAccepted(t *testing.T) {
	verbs := []string{"find", "get", "list", "search", "set", "close", "reopen"}
	for _, v := range verbs {
		r := NewRegistry()
		name := "eng_" + v + "_thing"
		err := r.Register(makeSpec(name, "placeholder description for verb test"))
		if err != nil {
			t.Errorf("verb %q should be accepted, got: %v", v, err)
		}
	}
}

func TestRegister_EmptyObjectSegmentRejectsName(t *testing.T) {
	r := NewRegistry()
	// "eng_get_" has no object segment
	err := r.Register(makeSpec("eng_get_", "some valid description here ok"))
	if err == nil {
		t.Fatal("expected error for missing object segment, got nil")
		return
	}
}

func TestRegister_ObjectStartsWithDigitRejectsName(t *testing.T) {
	r := NewRegistry()
	err := r.Register(makeSpec("eng_get_1thing", "some valid description here ok"))
	if err == nil {
		t.Fatal("expected error for object starting with digit, got nil")
		return
	}
}

// ---------------------------------------------------------------------------
// Register — description validation
// ---------------------------------------------------------------------------

func TestRegister_ShortDescriptionRejected(t *testing.T) {
	r := NewRegistry()
	// 9 chars — below the 10-char minimum
	err := r.Register(makeSpec("eng_get_node", "too short"))
	if err == nil {
		t.Fatal("expected error for description < 10 chars, got nil")
		return
	}
}

func TestRegister_ExactlyTenCharDescriptionAccepted(t *testing.T) {
	r := NewRegistry()
	err := r.Register(makeSpec("eng_get_node", "1234567890"))
	if err != nil {
		t.Fatalf("expected 10-char description to be accepted, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Register — duplicate rejection
// ---------------------------------------------------------------------------

func TestRegister_DuplicateNameRejected(t *testing.T) {
	r := NewRegistry()
	spec := makeSpec("eng_get_node", "gets a node by its unique identifier")
	if err := r.Register(spec); err != nil {
		t.Fatalf("first registration failed unexpectedly: %v", err)
	}
	if err := r.Register(spec); err == nil {
		t.Fatal("expected error for duplicate name, got nil")
		return
	}
}

// ---------------------------------------------------------------------------
// Dispatch
// ---------------------------------------------------------------------------

func TestDispatch_RoutesToHandler(t *testing.T) {
	r := NewRegistry()
	called := false
	spec := ToolSpec{
		Name:        "eng_find_symbol",
		Description: "finds a symbol by name in the graph",
		Handler: func(_ context.Context, _ domain.Actor, _ json.RawMessage) (any, *RPCError) {
			called = true
			return "ok", nil
		},
	}
	if err := r.Register(spec); err != nil {
		t.Fatalf("register: %v", err)
	}

	req := &Request{Method: "eng_find_symbol"}
	_, rpcErr := r.Dispatch(context.Background(), domain.Actor{ID: "human:test", Kind: domain.ActorKindHuman}, req)
	if rpcErr != nil {
		t.Fatalf("unexpected rpc error: %+v", rpcErr)
	}
	if !called {
		t.Fatal("handler was not called")
	}
}

// TestDispatch_ToolsListReturnsCatalog pins solov2-kw4: tools/list is
// recognised and returns every registered tool's name/description.
func TestDispatch_ToolsListReturnsCatalog(t *testing.T) {
	r := NewRegistry()
	r.MustRegister(ToolSpec{Name: "eng_get_node", Description: "fetch a graph node",
		Handler: func(context.Context, domain.Actor, json.RawMessage) (any, *RPCError) { return nil, nil }})
	r.MustRegister(ToolSpec{Name: "eng_find_symbol", Description: "find symbol by name",
		Handler: func(context.Context, domain.Actor, json.RawMessage) (any, *RPCError) { return nil, nil }})

	result, rpcErr := r.Dispatch(context.Background(), domain.Actor{}, &Request{Method: "tools/list"})
	if rpcErr != nil {
		t.Fatalf("tools/list error: %+v", rpcErr)
	}
	resp, ok := result.(ToolListResponse)
	if !ok {
		t.Fatalf("unexpected result type: %T", result)
	}
	if len(resp.Tools) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(resp.Tools))
	}
	if resp.Tools[0].Name != "eng_find_symbol" || resp.Tools[1].Name != "eng_get_node" {
		t.Errorf("expected sorted [eng_find_symbol, eng_get_node]; got %+v", resp.Tools)
	}
}

// TestDispatch_ToolsCallRoutesByName pins solov2-kw4: tools/call with
// {"name":"eng_find_symbol","arguments":{...}} dispatches to the tool
// handler with the unwrapped arguments.
func TestDispatch_ToolsCallRoutesByName(t *testing.T) {
	r := NewRegistry()
	var gotParams json.RawMessage
	r.MustRegister(ToolSpec{
		Name: "eng_find_symbol", Description: "find symbol by name",
		Handler: func(_ context.Context, _ domain.Actor, p json.RawMessage) (any, *RPCError) {
			gotParams = p
			return "found", nil
		},
	})

	args := json.RawMessage(`{"symbol":"Foo","repo_id":"r","branch":"main"}`)
	wrapped, _ := json.Marshal(map[string]any{"name": "eng_find_symbol", "arguments": json.RawMessage(args)})
	result, rpcErr := r.Dispatch(context.Background(), domain.Actor{}, &Request{Method: "tools/call", Params: wrapped})
	if rpcErr != nil {
		t.Fatalf("tools/call error: %+v", rpcErr)
	}
	if result != "found" {
		t.Errorf("expected handler return 'found', got %v", result)
	}
	if string(gotParams) != string(args) {
		t.Errorf("handler got params %s, want %s", gotParams, args)
	}
}

func TestDispatch_ToolsCallUnknownToolReturnsNotFound(t *testing.T) {
	r := NewRegistry()
	wrapped, _ := json.Marshal(map[string]any{"name": "eng_get_missing", "arguments": json.RawMessage(`{}`)})
	_, rpcErr := r.Dispatch(context.Background(), domain.Actor{}, &Request{Method: "tools/call", Params: wrapped})
	if rpcErr == nil || rpcErr.Code != CodeMethodNotFound {
		t.Fatalf("expected MethodNotFound, got %+v", rpcErr)
	}
}

func TestDispatch_UnknownMethodReturnsMethodNotFound(t *testing.T) {
	r := NewRegistry()
	req := &Request{Method: "eng_get_unknown_xyz"}
	_, rpcErr := r.Dispatch(context.Background(), domain.Actor{ID: "human:test", Kind: domain.ActorKindHuman}, req)
	if rpcErr == nil {
		t.Fatal("expected RPCError for unknown method, got nil")
		return
	}
	if rpcErr.Code != CodeMethodNotFound {
		t.Fatalf("expected code %d, got %d", CodeMethodNotFound, rpcErr.Code)
	}
}

// ---------------------------------------------------------------------------
// Names
// ---------------------------------------------------------------------------

func TestNames_ReturnsSortedList(t *testing.T) {
	r := NewRegistry()
	for _, name := range []string{"eng_search_aaaa", "eng_get_node", "eng_find_symbol"} {
		if err := r.Register(makeSpec(name, "placeholder description for names test")); err != nil {
			t.Fatalf("register %q: %v", name, err)
		}
	}
	names := r.Names()
	if len(names) != 3 {
		t.Fatalf("expected 3 names, got %d", len(names))
	}
	want := []string{"eng_find_symbol", "eng_get_node", "eng_search_aaaa"}
	for i, w := range want {
		if names[i] != w {
			t.Errorf("names[%d] = %q, want %q", i, names[i], w)
		}
	}
}

// ---------------------------------------------------------------------------
// Handle — satisfies Handler interface
// ---------------------------------------------------------------------------

func TestHandle_DelegatesToDispatch(t *testing.T) {
	r := NewRegistry()
	spec := ToolSpec{
		Name:        "eng_get_node",
		Description: "gets a node by its unique identifier in the graph",
		Handler: func(_ context.Context, _ domain.Actor, _ json.RawMessage) (any, *RPCError) {
			return "handled", nil
		},
	}
	if err := r.Register(spec); err != nil {
		t.Fatalf("register: %v", err)
	}

	req := &Request{Method: "eng_get_node"}
	result, rpcErr := r.Handle(context.Background(), domain.Actor{ID: "agent:test", Kind: domain.ActorKindAgent}, req)
	if rpcErr != nil {
		t.Fatalf("unexpected rpc error: %+v", rpcErr)
	}
	if result != "handled" {
		t.Fatalf("expected result 'handled', got %v", result)
	}
}

// ---------------------------------------------------------------------------
// MustRegister
// ---------------------------------------------------------------------------

func TestMustRegister_PanicsOnBadSpec(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for bad spec, got none")
			return
		}
	}()
	r := NewRegistry()
	r.MustRegister(makeSpec("bad_name", "description long enough here"))
}

// ---------------------------------------------------------------------------
// Contract test: all m1.06 tools pass validation
// ---------------------------------------------------------------------------

func TestAllM106ToolsPassValidation(t *testing.T) {
	tools := []string{
		"eng_find_symbol",
		"eng_get_node",
		"eng_get_call_chain",
		"eng_get_file_nodes",
		"eng_get_current_repo",
		"eng_list_repos",
		"eng_get_repo",
		"eng_get_status",
		"eng_get_config",
	}
	r := NewRegistry()
	for _, name := range tools {
		err := r.Register(ToolSpec{
			Name:        name,
			Description: "placeholder description for contract test",
			Handler:     func(_ context.Context, _ domain.Actor, _ json.RawMessage) (any, *RPCError) { return nil, nil },
		})
		if err != nil {
			t.Errorf("tool %q failed validation: %v", name, err)
		}
	}
}
