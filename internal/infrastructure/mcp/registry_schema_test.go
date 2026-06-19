// SPDX-License-Identifier: AGPL-3.0-only

package mcp

import (
	"encoding/json"
	"sort"
	"testing"
)

func TestRegister_NoSchemaRegistersWithNilSchemas(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(makeSpec("eng_get_node", "gets a node by identifier")); err != nil {
		t.Fatalf("register: %v", err)
	}
	spec, ok := r.Spec("eng_get_node")
	if !ok {
		t.Fatal("expected spec to be found")
	}
	if spec.InputSchema != nil {
		t.Errorf("expected nil InputSchema, got %s", spec.InputSchema)
	}
	if spec.OutputSchema != nil {
		t.Errorf("expected nil OutputSchema, got %s", spec.OutputSchema)
	}
}

func TestSpec_UnknownReturnsFalse(t *testing.T) {
	r := NewRegistry()
	if _, ok := r.Spec("eng_get_missing"); ok {
		t.Fatal("expected ok=false for unknown tool")
	}
}

func TestRegister_MalformedSchemaRejected(t *testing.T) {
	r := NewRegistry()
	spec := makeSpec("eng_get_node", "gets a node by identifier")
	spec.InputSchema = json.RawMessage(`{`)
	if err := r.Register(spec); err == nil {
		t.Fatal("expected error for malformed InputSchema, got nil")
		return
	}
}

func TestRegister_MalformedOutputSchemaRejected(t *testing.T) {
	r := NewRegistry()
	spec := makeSpec("eng_get_node", "gets a node by identifier")
	spec.OutputSchema = json.RawMessage(`{"type":`)
	if err := r.Register(spec); err == nil {
		t.Fatal("expected error for malformed OutputSchema, got nil")
		return
	}
}

func schemaProps(t *testing.T, raw json.RawMessage) (string, []string) {
	t.Helper()
	if !json.Valid(raw) {
		t.Fatalf("schema is not valid JSON: %s", raw)
	}
	var obj struct {
		Type       string                     `json:"type"`
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("unmarshal schema: %v", err)
	}
	keys := make([]string, 0, len(obj.Properties))
	for k := range obj.Properties {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return obj.Type, keys
}

func assertSchemaKeys(t *testing.T, raw json.RawMessage, wantKeys []string) {
	t.Helper()
	typ, keys := schemaProps(t, raw)
	if typ != "object" {
		t.Errorf("expected type=object, got %q", typ)
	}
	sort.Strings(wantKeys)
	if len(keys) != len(wantKeys) {
		t.Fatalf("properties keys = %v, want %v", keys, wantKeys)
	}
	for i := range keys {
		if keys[i] != wantKeys[i] {
			t.Errorf("properties keys = %v, want %v", keys, wantKeys)
			return
		}
	}
}

func TestStateChangingToolsPublishSchemas(t *testing.T) {
	// Task tools are registered dynamically rather than at daemon startup, so we exclude them from the static schema registry tests.
	r := NewRegistry()
	RegisterFindingTools(r, nil, nil, nil)
	RegisterSuppressionTools(r, nil, nil, nil)

	cases := []struct {
		name      string
		inputKeys []string
		outKeys   []string
	}{
		{
			name:      "eng_close_finding",
			inputKeys: []string{"finding_id", "branch", "repo_id", "reason"},
			outKeys:   []string{"finding_id", "branch", "state"},
		},
		{
			name:      "eng_suppress_finding",
			inputKeys: []string{"finding_id", "branch", "repo_id", "reason", "scope", "expires_at"},
			outKeys:   []string{"suppression_id", "finding_id", "branch", "scope"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec, ok := r.Spec(tc.name)
			if !ok {
				t.Fatalf("tool %q not registered", tc.name)
			}
			if len(spec.InputSchema) == 0 {
				t.Fatal("InputSchema is empty")
			}
			if len(spec.OutputSchema) == 0 {
				t.Fatal("OutputSchema is empty")
			}
			assertSchemaKeys(t, spec.InputSchema, tc.inputKeys)
			assertSchemaKeys(t, spec.OutputSchema, tc.outKeys)
		})
	}
}
