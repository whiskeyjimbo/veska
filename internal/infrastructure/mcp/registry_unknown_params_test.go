package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

// schemaWithProps builds an inputSchema with the given property names.
func schemaWithProps(props ...string) json.RawMessage {
	m := map[string]any{
		"type":       "object",
		"properties": map[string]any{},
	}
	for _, p := range props {
		m["properties"].(map[string]any)[p] = map[string]any{"type": "string"}
	}
	b, _ := json.Marshal(m)
	return b
}

func registerWithSchema(t *testing.T, r *Registry, name string, schema json.RawMessage) {
	t.Helper()
	r.MustRegister(ToolSpec{
		Name:        name,
		Description: "test tool for unknown-param rejection",
		InputSchema: schema,
		Handler: func(_ context.Context, _ domain.Actor, _ json.RawMessage) (any, *RPCError) {
			return "ok", nil
		},
	})
}

// TestDispatch_FlatUnknownParamRejected:
// Passing an unknown top-level param via the flat method form must
// return -32602 naming the offending key.
func TestDispatch_FlatUnknownParamRejected(t *testing.T) {
	r := NewRegistry()
	registerWithSchema(t, r, "eng_find_changed_symbols", schemaWithProps("repo_id", "branch", "ref_a", "ref_b"))

	params := json.RawMessage(`{"repo_id":"r","branch":"main","totally_made_up":"x"}`)
	_, rpcErr := r.Dispatch(context.Background(), domain.Actor{}, &Request{Method: "eng_find_changed_symbols", Params: params})
	if rpcErr == nil {
		t.Fatal("expected RPCError for unknown param, got nil")
	}
	if rpcErr.Code != CodeInvalidParams {
		t.Errorf("expected code %d, got %d", CodeInvalidParams, rpcErr.Code)
	}
	if !strings.Contains(rpcErr.Message, "totally_made_up") {
		t.Errorf("expected error message to name 'totally_made_up'; got %q", rpcErr.Message)
	}
}

// TestDispatch_ToolsCallUnknownParamRejected — same behaviour via tools/call.
func TestDispatch_ToolsCallUnknownParamRejected(t *testing.T) {
	r := NewRegistry()
	registerWithSchema(t, r, "eng_get_status", schemaWithProps())

	args := json.RawMessage(`{"surprise":"y"}`)
	wrapped, _ := json.Marshal(map[string]any{"name": "eng_get_status", "arguments": json.RawMessage(args)})
	_, rpcErr := r.Dispatch(context.Background(), domain.Actor{}, &Request{Method: "tools/call", Params: wrapped})
	if rpcErr == nil {
		t.Fatal("expected RPCError, got nil")
	}
	if rpcErr.Code != CodeInvalidParams {
		t.Errorf("expected -32602, got %d", rpcErr.Code)
	}
	if !strings.Contains(rpcErr.Message, "surprise") {
		t.Errorf("expected message to name 'surprise'; got %q", rpcErr.Message)
	}
}

// TestDispatch_KnownParamsAccepted — sanity: known keys still dispatch.
func TestDispatch_KnownParamsAccepted(t *testing.T) {
	r := NewRegistry()
	registerWithSchema(t, r, "eng_find_changed_symbols", schemaWithProps("repo_id", "branch", "ref_a", "ref_b"))

	params := json.RawMessage(`{"repo_id":"r","branch":"main","ref_a":"HEAD~1","ref_b":"HEAD"}`)
	res, rpcErr := r.Dispatch(context.Background(), domain.Actor{}, &Request{Method: "eng_find_changed_symbols", Params: params})
	if rpcErr != nil {
		t.Fatalf("unexpected rpc error: %+v", rpcErr)
	}
	if res != "ok" {
		t.Errorf("expected handler result 'ok', got %v", res)
	}
}

// TestDispatch_EmptyParamsAccepted — empty / no params must not error.
func TestDispatch_EmptyParamsAccepted(t *testing.T) {
	r := NewRegistry()
	registerWithSchema(t, r, "eng_get_status", schemaWithProps())

	// nil params
	if _, rpcErr := r.Dispatch(context.Background(), domain.Actor{}, &Request{Method: "eng_get_status"}); rpcErr != nil {
		t.Errorf("nil params: unexpected error %+v", rpcErr)
	}
	// empty object
	if _, rpcErr := r.Dispatch(context.Background(), domain.Actor{}, &Request{Method: "eng_get_status", Params: json.RawMessage(`{}`)}); rpcErr != nil {
		t.Errorf("empty object: unexpected error %+v", rpcErr)
	}
}

// TestDispatch_ToolWithoutSchemaSkipsValidation — tools that don't publish
// an inputSchema must not block on unknown keys (no schema = no contract).
func TestDispatch_ToolWithoutSchemaSkipsValidation(t *testing.T) {
	r := NewRegistry()
	r.MustRegister(ToolSpec{
		Name:        "eng_get_legacy",
		Description: "legacy tool without a schema",
		Handler: func(_ context.Context, _ domain.Actor, _ json.RawMessage) (any, *RPCError) {
			return "ok", nil
		},
	})

	params := json.RawMessage(`{"anything":"goes"}`)
	if _, rpcErr := r.Dispatch(context.Background(), domain.Actor{}, &Request{Method: "eng_get_legacy", Params: params}); rpcErr != nil {
		t.Errorf("tool without schema should accept any params; got %+v", rpcErr)
	}
}

// TestDispatch_NonObjectParamsRejected — params declared object but given an
// array/string should be rejected with -32602.
func TestDispatch_NonObjectParamsRejected(t *testing.T) {
	r := NewRegistry()
	registerWithSchema(t, r, "eng_get_status", schemaWithProps("repo_id"))

	params := json.RawMessage(`["not","an","object"]`)
	_, rpcErr := r.Dispatch(context.Background(), domain.Actor{}, &Request{Method: "eng_get_status", Params: params})
	if rpcErr == nil {
		t.Fatal("expected RPCError for non-object params, got nil")
	}
	if rpcErr.Code != CodeInvalidParams {
		t.Errorf("expected -32602, got %d", rpcErr.Code)
	}
}

// TestDispatch_TransportInjectedCwdAllowed — the veska-mcp shim injects
// "cwd" into every eng_* request even when the tool's schema doesn't list
// it. Validation must accept the key so the shim path keeps working
// ( + ).
func TestDispatch_TransportInjectedCwdAllowed(t *testing.T) {
	r := NewRegistry()
	// Schema with no cwd declared.
	registerWithSchema(t, r, "eng_get_status", schemaWithProps())
	params := json.RawMessage(`{"cwd":"/abs/work"}`)
	if _, rpcErr := r.Dispatch(context.Background(), domain.Actor{}, &Request{Method: "eng_get_status", Params: params}); rpcErr != nil {
		t.Errorf("transport-injected cwd should pass validation; got %+v", rpcErr)
	}
}

// TestDispatch_ToolsListUnchanged — tools/list takes no schema params and
// must not be subjected to validation.
func TestDispatch_ToolsListUnchanged(t *testing.T) {
	r := NewRegistry()
	registerWithSchema(t, r, "eng_get_status", schemaWithProps())
	if _, rpcErr := r.Dispatch(context.Background(), domain.Actor{}, &Request{Method: "tools/list"}); rpcErr != nil {
		t.Fatalf("tools/list: unexpected error: %+v", rpcErr)
	}
}
