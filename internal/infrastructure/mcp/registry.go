package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/observability"
	"go.opentelemetry.io/otel/trace"
)

// toolNamePattern enforces the eng_<verb>_<object> naming convention.
// The object segment must start with a lowercase letter and contain only
// lowercase letters, digits, and underscores.
var toolNamePattern = regexp.MustCompile(`^eng_(find|get|list|search|set|close|reopen|suppress)_[a-z][a-z0-9_]*$`)

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

// tracerProvider returns the configured provider or a noop if nil.
func (r *Registry) tracerProvider() observability.TracerProvider {
	if r.tp == nil {
		return trace.NewNoopTracerProvider()
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
		return fmt.Errorf("mcp: tool name %q does not match eng_<verb>_<object> pattern (allowed verbs: find,get,list,search,set,close,reopen,suppress; object must start with [a-z])", spec.Name)
	}
	if len(spec.Description) < minDescriptionLen {
		return fmt.Errorf("mcp: tool %q description is %d chars, minimum is %d", spec.Name, len(spec.Description), minDescriptionLen)
	}
	if _, exists := r.tools[spec.Name]; exists {
		return fmt.Errorf("mcp: tool %q is already registered", spec.Name)
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
func (r *Registry) Dispatch(ctx context.Context, actor domain.Actor, req *Request) (any, *RPCError) {
	spec, ok := r.tools[req.Method]
	if !ok {
		return nil, &RPCError{
			Code:    CodeMethodNotFound,
			Message: fmt.Sprintf("method not found: %s", req.Method),
		}
	}
	ctx, span := observability.StartSpan(ctx, r.tracerProvider(), "mcp."+req.Method)
	defer span.End()
	return spec.Handler(ctx, actor, req.Params)
}

// Handle satisfies the Handler interface so Registry can be passed directly
// to NewServer. It delegates to Dispatch.
func (r *Registry) Handle(ctx context.Context, actor domain.Actor, req *Request) (any, *RPCError) {
	return r.Dispatch(ctx, actor, req)
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
