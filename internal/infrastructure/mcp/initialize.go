package mcp

import (
	"encoding/json"
	"runtime/debug"
)

// defaultProtocolVersion is the MCP protocol version the server advertises
// when the client omits one in initialize params. Strict clients (notably
// claude-desktop) require initialize to succeed before issuing tools/list;
// see https://spec.modelcontextprotocol.io/specification/basic/lifecycle/.
const defaultProtocolVersion = "2024-11-05"

// InitializeResult is the MCP-spec initialize response.
type InitializeResult struct {
	ProtocolVersion string               `json:"protocolVersion"`
	Capabilities    map[string]any       `json:"capabilities"`
	ServerInfo      InitializeServerInfo `json:"serverInfo"`
}

// InitializeServerInfo identifies the server to the client.
type InitializeServerInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// handleInitialize builds the MCP initialize response. The client may
// declare its preferred protocolVersion; we echo it back rather than
// imposing one so older clients that pin an earlier version still proceed.
func handleInitialize(params json.RawMessage) InitializeResult {
	protoVer := defaultProtocolVersion
	if len(params) > 0 {
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		if err := json.Unmarshal(params, &p); err == nil && p.ProtocolVersion != "" {
			protoVer = p.ProtocolVersion
		}
	}
	return InitializeResult{
		ProtocolVersion: protoVer,
		Capabilities:    map[string]any{"tools": map[string]any{}},
		ServerInfo: InitializeServerInfo{
			Name:    "veska",
			Version: buildVersion(),
		},
	}
}

// buildVersion mirrors cmd/veska/version.go's resolution: prefer the module
// version, fall back to "dev" when running from a working tree.
func buildVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "dev"
	}
	v := info.Main.Version
	if v == "" || v == "(devel)" {
		return "dev"
	}
	return v
}
