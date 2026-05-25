package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/observability"
	"go.opentelemetry.io/otel/trace/noop"
)

// toolNamePattern enforces the eng_<verb>_<object> naming convention.
// The object segment must start with a lowercase letter and contain only
// lowercase letters, digits, and underscores.
var toolNamePattern = regexp.MustCompile(`^eng_(find|get|list|search|set|close|reopen|suppress|add|remove|promote)_[a-z][a-z0-9_]*$`)

const minDescriptionLen = 10

// ToolHandler is called when a tool request arrives.
// actor carries the full attribution stamp so handlers can apply trust,
// rate-limit, or audit policies based on who is calling.
type ToolHandler func(ctx context.Context, actor domain.Actor, params json.RawMessage) (any, *RPCError)

// ToolSpec describes one MCP tool registered with the server.
type ToolSpec struct {
	// Name is the tool's MCP identifier: must match eng_<verb>_<object>.
	Name string
	// Description is a human-readable summary (≥ 10 chars).
	Description string
	// IncludesStaging is true if this tool reads through the staging overlay.
	IncludesStaging bool
	// Handler processes the tool call.
	Handler ToolHandler
	// InputSchema is an optional JSON Schema (draft 2020-12) describing the
	// tool's params object. Empty when the tool publishes no schema.
	InputSchema json.RawMessage
	// OutputSchema is an optional JSON Schema describing the tool's result
	// shape. Empty when the tool publishes no schema.
	OutputSchema json.RawMessage
}

// Registry holds the set of registered tools and dispatches incoming tool calls.
//
// Concurrency note: Register is intended to be called exclusively at startup,
// before serving begins. The map is never written after the first call to
// Dispatch or Handle; those methods are safe for concurrent use as they only
// read the map. If Register is called concurrently with itself or with
// Dispatch/Handle the behaviour is undefined.
type Registry struct {
	tools map[string]ToolSpec
	tp    observability.TracerProvider
}

// NewRegistry creates an empty registry.
func NewRegistry() *Registry {
	return &Registry{tools: make(map[string]ToolSpec)}
}

// SetTracerProvider installs a TracerProvider for mcp.<tool> spans.
// If not called (or called with nil), a noop provider is used.
func (r *Registry) SetTracerProvider(tp observability.TracerProvider) {
	r.tp = tp
}

// TracerProvider returns the installed TracerProvider, or nil if none has
// been set. It is the read companion to SetTracerProvider.
func (r *Registry) TracerProvider() observability.TracerProvider {
	return r.tp
}

// tracerProvider returns the configured provider or a noop if nil.
func (r *Registry) tracerProvider() observability.TracerProvider {
	if r.tp == nil {
		return noop.NewTracerProvider()
	}
	return r.tp
}

// Register adds a tool. Returns an error if:
//   - name does not match eng_<verb>_<object> pattern
//   - verb is not in the closed set (find/get/list/search/set/close/reopen)
//   - description is shorter than 10 characters
//   - a tool with the same name is already registered
//
// Register is intended to be called at startup only; it is not safe for
// concurrent use after serving begins.
func (r *Registry) Register(spec ToolSpec) error {
	if !toolNamePattern.MatchString(spec.Name) {
		return fmt.Errorf("mcp: tool name %q does not match eng_<verb>_<object> pattern (allowed verbs: find,get,list,search,set,close,reopen,suppress,add,remove,promote; object must start with [a-z])", spec.Name)
	}
	if len(spec.Description) < minDescriptionLen {
		return fmt.Errorf("mcp: tool %q description is %d chars, minimum is %d", spec.Name, len(spec.Description), minDescriptionLen)
	}
	if _, exists := r.tools[spec.Name]; exists {
		return fmt.Errorf("mcp: tool %q is already registered", spec.Name)
	}
	// Schemas are optional, but a non-empty schema must be valid JSON.
	if len(spec.InputSchema) > 0 && !json.Valid(spec.InputSchema) {
		return fmt.Errorf("mcp: tool %q InputSchema is not valid JSON", spec.Name)
	}
	if len(spec.OutputSchema) > 0 && !json.Valid(spec.OutputSchema) {
		return fmt.Errorf("mcp: tool %q OutputSchema is not valid JSON", spec.Name)
	}
	r.tools[spec.Name] = spec
	return nil
}

// MustRegister panics if Register returns an error. Use at init time.
func (r *Registry) MustRegister(spec ToolSpec) {
	if err := r.Register(spec); err != nil {
		panic(err)
	}
}

// Dispatch routes a JSON-RPC request to the matching tool handler.
// Returns MethodNotFound (-32601) if no tool matches.
// Safe for concurrent use provided no further Register calls occur.
//
// Three method-name forms are accepted (solov2-kw4):
//
//   - "eng_<verb>_<object>" — flat dialect, original; method == tool name.
//   - "tools/list"          — MCP standard discovery; returns the catalog.
//   - "tools/call"          — MCP standard invocation; req.Params must
//     carry {"name": "<tool>", "arguments": {...}}.
//
// Existing flat callers (the veska-mcp shim, the CLI's callMCP) keep
// working unchanged; new AI clients speaking standard MCP get the
// expected surface.
func (r *Registry) Dispatch(ctx context.Context, actor domain.Actor, req *Request) (any, *RPCError) {
	switch req.Method {
	case "tools/list":
		return r.handleToolsList(), nil
	case "tools/call":
		return r.handleToolsCall(ctx, actor, req.Params)
	}
	spec, ok := r.tools[req.Method]
	if !ok {
		return nil, &RPCError{
			Code:    CodeMethodNotFound,
			Message: fmt.Sprintf("method not found: %s", req.Method),
		}
	}
	if rpcErr := validateAgainstSchema(req.Method, spec.InputSchema, req.Params); rpcErr != nil {
		return nil, rpcErr
	}
	ctx, span := observability.StartSpan(ctx, r.tracerProvider(), "mcp."+req.Method)
	defer span.End()
	return spec.Handler(ctx, actor, req.Params)
}

// validateAgainstSchema enforces that, when a tool publishes an inputSchema
// with declared "properties", incoming params:
//   - decode to a JSON object (or are empty/null), and
//   - contain no top-level keys outside the schema's properties set.
//
// Unknown keys yield CodeInvalidParams (-32602) with the offending key name,
// closing the silent-drop bug (solov2-9bzq). Tools without an inputSchema
// publish no contract and are not validated here.
func validateAgainstSchema(method string, schema, params json.RawMessage) *RPCError {
	if len(schema) == 0 {
		return nil
	}
	// Empty or null params — nothing to check.
	trimmed := bytesTrimSpace(params)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nil
	}
	var s struct {
		Type       string                      `json:"type"`
		Properties *map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(schema, &s); err != nil {
		// Malformed schemas are caught at Register-time; ignore here.
		return nil
	}
	// Validation only applies to object schemas that declare a properties map.
	// A schema without a "properties" key (nil pointer) publishes no contract
	// over its keys and is left alone — back-compat with pass-through tools.
	if s.Properties == nil {
		return nil
	}
	props := *s.Properties
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(params, &obj); err != nil {
		return &RPCError{
			Code:    CodeInvalidParams,
			Message: fmt.Sprintf("%s: params must be a JSON object: %v", method, err),
		}
	}
	for k := range obj {
		if _, ok := props[k]; ok {
			continue
		}
		if transportInjectedKeys[k] {
			// Keys injected by veska-mcp at the transport layer (solov2-ktz0).
			// They're not part of the tool's published contract but every
			// request passing through the shim carries them; rejecting them
			// would break the shim path while validating the daemon-direct
			// path. Pass through silently — handlers that care extract them
			// via cwdFromParams, ones that don't ignore them.
			continue
		}
		return &RPCError{
			Code:    CodeInvalidParams,
			Message: fmt.Sprintf("%s: unknown parameter %q (allowed: %s)", method, k, sortedKeys(props)),
		}
	}
	return nil
}

// transportInjectedKeys are top-level params the veska-mcp shim adds to
// every eng_* request (cwd today; the set may grow). They're treated as
// always-allowed during schema validation so the shim path doesn't fail
// validation for tools that don't otherwise declare them.
var transportInjectedKeys = map[string]bool{
	"cwd": true,
}

// bytesTrimSpace trims ASCII whitespace from a json.RawMessage without
// allocating; used to detect empty/null params cheaply.
func bytesTrimSpace(b json.RawMessage) json.RawMessage {
	i, j := 0, len(b)
	for i < j {
		switch b[i] {
		case ' ', '\t', '\n', '\r':
			i++
			continue
		}
		break
	}
	for j > i {
		switch b[j-1] {
		case ' ', '\t', '\n', '\r':
			j--
			continue
		}
		break
	}
	return b[i:j]
}

// sortedKeys returns m's keys joined with ", " in lexical order — used to
// build a deterministic "allowed: a, b, c" hint in validation errors.
func sortedKeys(m map[string]json.RawMessage) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ", ")
}

// ToolListEntry is one row in the tools/list response. Matches the MCP
// spec shape (name + description + inputSchema; outputSchema is included
// when the tool publishes one).
type ToolListEntry struct {
	Name         string          `json:"name"`
	Description  string          `json:"description"`
	InputSchema  json.RawMessage `json:"inputSchema,omitempty"`
	OutputSchema json.RawMessage `json:"outputSchema,omitempty"`
}

// ToolListResponse envelopes the catalog under the MCP-spec key "tools".
type ToolListResponse struct {
	Tools []ToolListEntry `json:"tools"`
}

func (r *Registry) handleToolsList() ToolListResponse {
	names := r.Names()
	out := make([]ToolListEntry, 0, len(names))
	for _, n := range names {
		spec := r.tools[n]
		out = append(out, ToolListEntry{
			Name:         spec.Name,
			Description:  spec.Description,
			InputSchema:  spec.InputSchema,
			OutputSchema: spec.OutputSchema,
		})
	}
	return ToolListResponse{Tools: out}
}

func (r *Registry) handleToolsCall(ctx context.Context, actor domain.Actor, raw json.RawMessage) (any, *RPCError) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, &RPCError{
			Code:    CodeInvalidParams,
			Message: fmt.Sprintf("tools/call: invalid params: %v", err),
		}
	}
	if p.Name == "" {
		return nil, &RPCError{
			Code:    CodeInvalidParams,
			Message: "tools/call: 'name' is required",
		}
	}
	spec, ok := r.tools[p.Name]
	if !ok {
		return nil, &RPCError{
			Code:    CodeMethodNotFound,
			Message: fmt.Sprintf("tools/call: tool not found: %s", p.Name),
		}
	}
	if rpcErr := validateAgainstSchema(p.Name, spec.InputSchema, p.Arguments); rpcErr != nil {
		return nil, rpcErr
	}
	ctx, span := observability.StartSpan(ctx, r.tracerProvider(), "mcp."+p.Name)
	defer span.End()
	return spec.Handler(ctx, actor, p.Arguments)
}

// Handle satisfies the Handler interface so Registry can be passed directly
// to NewServer. It delegates to Dispatch.
func (r *Registry) Handle(ctx context.Context, actor domain.Actor, req *Request) (any, *RPCError) {
	return r.Dispatch(ctx, actor, req)
}

// Spec returns the registered ToolSpec for name, and whether it was found.
// It is a read-only accessor, safe for concurrent use provided no further
// Register calls occur.
func (r *Registry) Spec(name string) (ToolSpec, bool) {
	spec, ok := r.tools[name]
	return spec, ok
}

// Names returns all registered tool names in sorted order.
func (r *Registry) Names() []string {
	names := make([]string, 0, len(r.tools))
	for name := range r.tools {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
