// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

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

func TestDispatch_EmptyParamsAccepted(t *testing.T) {
	r := NewRegistry()
	registerWithSchema(t, r, "eng_get_status", schemaWithProps())

	if _, rpcErr := r.Dispatch(context.Background(), domain.Actor{}, &Request{Method: "eng_get_status"}); rpcErr != nil {
		t.Errorf("nil params: unexpected error %+v", rpcErr)
	}
	if _, rpcErr := r.Dispatch(context.Background(), domain.Actor{}, &Request{Method: "eng_get_status", Params: json.RawMessage(`{}`)}); rpcErr != nil {
		t.Errorf("empty object: unexpected error %+v", rpcErr)
	}
}

// Tools that do not specify an input schema skip parameter validation to support legacy or dynamic message schemas.
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

// Parameter payloads that are not JSON objects are rejected with invalid parameters error.
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

// The MCP transport layer implicitly injects a 'cwd' parameter, so validation must accept it even when the tool schema does not list it.
func TestDispatch_TransportInjectedCwdAllowed(t *testing.T) {
	r := NewRegistry()
	registerWithSchema(t, r, "eng_get_status", schemaWithProps())
	params := json.RawMessage(`{"cwd":"/abs/work"}`)
	if _, rpcErr := r.Dispatch(context.Background(), domain.Actor{}, &Request{Method: "eng_get_status", Params: params}); rpcErr != nil {
		t.Errorf("transport-injected cwd should pass validation; got %+v", rpcErr)
	}
}

// The tools/list method accepts no parameters and is excluded from parameter validation.
func TestDispatch_ToolsListUnchanged(t *testing.T) {
	r := NewRegistry()
	registerWithSchema(t, r, "eng_get_status", schemaWithProps())
	if _, rpcErr := r.Dispatch(context.Background(), domain.Actor{}, &Request{Method: "tools/list"}); rpcErr != nil {
		t.Fatalf("tools/list: unexpected error: %+v", rpcErr)
	}
}
