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
