// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package mcp

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

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

func TestRegister_ValidSpec(t *testing.T) {
	r := NewRegistry()
	err := r.Register(makeSpec("eng_find_symbol", "finds a symbol by name in the graph"))
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
}

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

func TestRegister_ShortDescriptionRejected(t *testing.T) {
	r := NewRegistry()
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

func TestDispatch_ToolsListReturnsCatalog(t *testing.T) {
	r := NewRegistry()
	r.MustRegister(ToolSpec{
		Name: "eng_get_node", Description: "fetch a graph node",
		Handler: func(context.Context, domain.Actor, json.RawMessage) (any, *RPCError) { return nil, nil },
	})
	r.MustRegister(ToolSpec{
		Name: "eng_find_symbol", Description: "find symbol by name",
		Handler: func(context.Context, domain.Actor, json.RawMessage) (any, *RPCError) { return nil, nil },
	})

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

// Strict MCP clients require a successful initialize handshake before executing other tools, so the registry must support this endpoint.
func TestDispatch_InitializeReturnsServerInfo(t *testing.T) {
	r := NewRegistry()
	params := json.RawMessage(`{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"test","version":"1"}}`)
	result, rpcErr := r.Dispatch(context.Background(), domain.Actor{}, &Request{Method: "initialize", Params: params})
	if rpcErr != nil {
		t.Fatalf("initialize error: %+v", rpcErr)
	}
	res, ok := result.(InitializeResult)
	if !ok {
		t.Fatalf("unexpected result type: %T", result)
	}
	if res.ProtocolVersion != "2024-11-05" {
		t.Errorf("ProtocolVersion = %q, want client's %q", res.ProtocolVersion, "2024-11-05")
	}
	if res.ServerInfo.Name != "veska" {
		t.Errorf("ServerInfo.Name = %q, want \"veska\"", res.ServerInfo.Name)
	}
	if res.ServerInfo.Version == "" {
		t.Errorf("ServerInfo.Version is empty")
	}
	if _, ok := res.Capabilities["tools"]; !ok {
		t.Errorf("Capabilities missing \"tools\": %+v", res.Capabilities)
	}
}

// If the client omits the protocol version during initialization, the server must supply a default protocol version instead of returning an empty string.
func TestDispatch_InitializeEmptyParamsDefaultsProtocol(t *testing.T) {
	r := NewRegistry()
	result, rpcErr := r.Dispatch(context.Background(), domain.Actor{}, &Request{Method: "initialize"})
	if rpcErr != nil {
		t.Fatalf("initialize error: %+v", rpcErr)
	}
	res := result.(InitializeResult)
	if res.ProtocolVersion != defaultProtocolVersion {
		t.Errorf("ProtocolVersion = %q, want default %q", res.ProtocolVersion, defaultProtocolVersion)
	}
}

// MCP lifecycle notifications must dispatch without returning an error so the connection handler can process them without sending a response.
func TestDispatch_NotificationsInitializedReturnsNoError(t *testing.T) {
	r := NewRegistry()
	result, rpcErr := r.Dispatch(context.Background(), domain.Actor{}, &Request{Method: "notifications/initialized"})
	if rpcErr != nil {
		t.Fatalf("notifications/initialized error: %+v", rpcErr)
	}
	if result != nil {
		t.Errorf("notifications/initialized result = %v, want nil", result)
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

// A tool exempt from CLI parity via ExemptDeferred must specify a justification reason so that parity lint tools and maintainers can evaluate when to upgrade it.
func TestRegister_CLIExemptDeferredRequiresReason(t *testing.T) {
	r := NewRegistry()
	spec := makeSpec("eng_get_widget", "gets the widget")
	spec.CLIExempt = ExemptDeferred
	if err := r.Register(spec); err == nil {
		t.Fatal("expected ExemptDeferred without ExemptReason to error")
	}

	spec.ExemptReason = "wrapping deferred until the use-case stabilises."
	if err := r.Register(spec); err != nil {
		t.Fatalf("ExemptDeferred with reason should register, got: %v", err)
	}
}

func TestRegister_CLIExemptOtherKindsNoReason(t *testing.T) {
	cases := []CLIExempt{ExemptInternal, ExemptAgentOnly}
	for _, k := range cases {
		r := NewRegistry()
		spec := makeSpec("eng_get_widget", "gets the widget")
		spec.CLIExempt = k
		if err := r.Register(spec); err != nil {
			t.Errorf("CLIExempt=%s should register without reason; got: %v", k, err)
		}
	}
}

// The Tools accessor must return a copy of the registered tools list so that external lints can inspect the configuration without mutating state.
func TestRegistry_ToolsReturnsSnapshot(t *testing.T) {
	r := NewRegistry()
	_ = r.Register(makeSpec("eng_find_symbol", "finds symbols"))
	_ = r.Register(makeSpec("eng_get_node", "gets a node by id"))
	out := r.Tools()
	if len(out) != 2 {
		t.Fatalf("Tools() = %d entries, want 2", len(out))
	}
	if out[0].Name >= out[1].Name {
		t.Errorf("Tools() not sorted by name: %v", []string{out[0].Name, out[1].Name})
	}
}
