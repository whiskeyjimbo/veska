// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package mcp

import (
	"encoding/json"
	"runtime/debug"
)

// defaultProtocolVersion is the fallback version used if the client does not specify one.
// Strict clients (such as Claude Desktop) require initialization to succeed before querying tools.
const defaultProtocolVersion = "2024-11-05"

type InitializeResult struct {
	ProtocolVersion string               `json:"protocolVersion"`
	Capabilities    map[string]any       `json:"capabilities"`
	ServerInfo      InitializeServerInfo `json:"serverInfo"`
}

type InitializeServerInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// handleInitialize handles the handshake by echoing the client's protocol version
// to maximize compatibility with older client versions.
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

// buildVersion returns the main module version, falling back to 'dev' when compiled from a working tree.
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
