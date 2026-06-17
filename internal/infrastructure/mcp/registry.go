package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/platform/observability"
	"go.opentelemetry.io/otel/trace/noop"
)

// toolNamePattern enforces the tool naming convention where names must start with 'eng_', followed by an approved verb, and an object segment.
var toolNamePattern = regexp.MustCompile(`^eng_(find|get|list|search|set|close|reopen|suppress|add|remove|promote|reindex)_[a-z][a-z0-9_]*$`)

const minDescriptionLen = 10

// ToolHandler processes incoming tool requests, using the actor parameters to apply security, rate-limiting, or auditing policies.
type ToolHandler func(ctx context.Context, actor domain.Actor, params json.RawMessage) (any, *RPCError)

type ToolSpec struct {
	Name            string
	Description     string
	IncludesStaging bool
	Handler         ToolHandler
	InputSchema     json.RawMessage
	OutputSchema    json.RawMessage

	// CLIExempt indicates that the tool is intentionally omitted from the CLI subcommands to prevent parity linter failures.
	CLIExempt CLIExempt
	// ExemptReason explains the exemption, and is required when CLIExempt is ExemptDeferred.
	ExemptReason string
}

// CLIExempt categorizes reasons why a tool does not have a corresponding CLI subcommand.
type CLIExempt int

const (
	CLIExemptNone CLIExempt = iota
	// ExemptInternal is for internal, debug, or test-only tools.
	ExemptInternal
	// ExemptAgentOnly is for tools that require conversational session state not supported by the CLI.
	ExemptAgentOnly
	// ExemptDeferred flags a tool that will be wrapped by a CLI subcommand later.
	ExemptDeferred
)

func (c CLIExempt) String() string {
	switch c {
	case CLIExemptNone:
		return "none"
	case ExemptInternal:
		return "internal"
	case ExemptAgentOnly:
		return "agent-only"
	case ExemptDeferred:
		return "deferred"
	default:
		return fmt.Sprintf("CLIExempt(%d)", int(c))
	}
}

// Registry holds the set of registered tools.
// Registry must have all tools registered at startup before serving begins; once serving starts, Dispatch and Handle are safe for concurrent read-only access.
type Registry struct {
	tools map[string]ToolSpec
	tp    observability.TracerProvider
}

func NewRegistry() *Registry {
	return &Registry{tools: make(map[string]ToolSpec)}
}

// SetTracerProvider registers the TracerProvider used for tool tracing spans.
func (r *Registry) SetTracerProvider(tp observability.TracerProvider) {
	r.tp = tp
}

func (r *Registry) TracerProvider() observability.TracerProvider {
	return r.tp
}

func (r *Registry) tracerProvider() observability.TracerProvider {
	if r.tp == nil {
		return noop.NewTracerProvider()
	}
	return r.tp
}

// Register adds a new tool to the registry and performs validation of the spec names, schemas, and exemptions.
func (r *Registry) Register(spec ToolSpec) error {
	if !toolNamePattern.MatchString(spec.Name) {
		return fmt.Errorf("mcp: tool name %q does not match eng_<verb>_<object> pattern (allowed verbs: find,get,list,search,set,close,reopen,suppress,add,remove,promote,reindex; object must start with [a-z])", spec.Name)
	}
	if len(spec.Description) < minDescriptionLen {
		return fmt.Errorf("mcp: tool %q description is %d chars, minimum is %d", spec.Name, len(spec.Description), minDescriptionLen)
	}
	if _, exists := r.tools[spec.Name]; exists {
		return fmt.Errorf("mcp: tool %q is already registered", spec.Name)
	}
	if len(spec.InputSchema) > 0 && !json.Valid(spec.InputSchema) {
		return fmt.Errorf("mcp: tool %q InputSchema is not valid JSON", spec.Name)
	}
	if len(spec.OutputSchema) > 0 && !json.Valid(spec.OutputSchema) {
		return fmt.Errorf("mcp: tool %q OutputSchema is not valid JSON", spec.Name)
	}
	if spec.CLIExempt == ExemptDeferred && spec.ExemptReason == "" {
		return fmt.Errorf("mcp: tool %q has CLIExempt=ExemptDeferred but no ExemptReason; either supply a reason or pick ExemptInternal/ExemptAgentOnly", spec.Name)
	}
	r.tools[spec.Name] = spec
	return nil
}

// Tools returns all registered specs sorted by name, which is used by the parity linter to walk the tool catalog.
func (r *Registry) Tools() []ToolSpec {
	names := make([]string, 0, len(r.tools))
	for name := range r.tools {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]ToolSpec, 0, len(names))
	for _, n := range names {
		out = append(out, r.tools[n])
	}
	return out
}

func (r *Registry) MustRegister(spec ToolSpec) {
	if err := r.Register(spec); err != nil {
		panic(err)
	}
}

// Dispatch routes a JSON-RPC request to the appropriate tool handler, supporting both standard MCP discovery/invocation protocols and legacy flat tool call formats.
func (r *Registry) Dispatch(ctx context.Context, actor domain.Actor, req *Request) (any, *RPCError) {
	switch req.Method {
	case "initialize":
		return handleInitialize(req.Params), nil
	case "notifications/initialized":
		return nil, nil
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

// validateAgainstSchema ensures incoming parameters contain no undocumented top-level keys if the tool publishes an input schema, returning CodeInvalidParams if unexpected parameters are encountered.
func validateAgainstSchema(method string, schema, params json.RawMessage) *RPCError {
	if len(schema) == 0 {
		return nil
	}
	trimmed := bytesTrimSpace(params)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nil
	}
	var s struct {
		Type       string                      `json:"type"`
		Properties *map[string]json.RawMessage `json:"properties"`
		Required   []string                    `json:"required"`
	}
	if err := json.Unmarshal(schema, &s); err != nil {
		return nil
	}
	if s.Properties == nil {
		return nil
	}
	props := *s.Properties
	required := make(map[string]bool, len(s.Required))
	for _, k := range s.Required {
		required[k] = true
	}
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
			// transportInjectedKeys are added by the veska-mcp shim and are ignored during validation to allow seamless shim and direct-path routing.
			continue
		}
		// We return validation errors that list both required and optional parameters to help callers resolve contract issues without fetching the full schema.
		return &RPCError{
			Code:    CodeInvalidParams,
			Message: fmt.Sprintf("%s: unknown parameter %q (allowed: %s)", method, k, sortedKeysAnnotated(props, required)),
		}
	}
	return nil
}

// transportInjectedKeys holds keys injected at the transport layer that should bypass schema validation.
var transportInjectedKeys = map[string]bool{
	"cwd": true,
}

// bytesTrimSpace trims ASCII whitespace from a json.RawMessage without allocating memory.
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

func sortedKeys(m map[string]json.RawMessage) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ", ")
}

// sortedKeysAnnotated lists required keys first, appending ' (required)' to help callers identify mandatory fields.
func sortedKeysAnnotated(m map[string]json.RawMessage, required map[string]bool) string {
	if len(required) == 0 {
		return sortedKeys(m)
	}
	req := make([]string, 0, len(required))
	opt := make([]string, 0, len(m))
	for k := range m {
		if required[k] {
			req = append(req, k+" (required)")
		} else {
			opt = append(opt, k)
		}
	}
	sort.Strings(req)
	sort.Strings(opt)
	return strings.Join(append(req, opt...), ", ")
}

type ToolListEntry struct {
	Name         string          `json:"name"`
	Description  string          `json:"description"`
	InputSchema  json.RawMessage `json:"inputSchema,omitempty"`
	OutputSchema json.RawMessage `json:"outputSchema,omitempty"`
}

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

func (r *Registry) Handle(ctx context.Context, actor domain.Actor, req *Request) (any, *RPCError) {
	return r.Dispatch(ctx, actor, req)
}

func (r *Registry) Spec(name string) (ToolSpec, bool) {
	spec, ok := r.tools[name]
	return spec, ok
}

func (r *Registry) Names() []string {
	names := make([]string, 0, len(r.tools))
	for name := range r.tools {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
