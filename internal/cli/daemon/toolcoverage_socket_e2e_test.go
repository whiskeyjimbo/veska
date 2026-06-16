//go:build socket_e2e

package daemon

// On-demand socket end-to-end harness.
// This file is build-tag gated behind `socket_e2e` so it stays OUT of the
// default `go test` / `make all` path — it exercises the REAL daemon socket
// server (two Unix sockets, line-delimited JSON-RPC) rather than the in-process
// Registry.Call shortcut the coverage suite uses. Run it with `make
// tool-test-e2e` or:
//	go test -tags "sqlite_fts5 socket_e2e" -run TestSocketE2E./internal/cli/daemon/.
// It is intentionally NOT exhaustive: it proves the socket round-trip reaches
// the registry and returns real fixture facts for one tool (eng_get_node). The
// per-tool exhaustiveness lives in the in-process coverage suite.

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/mcp"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/mcp/coverage"
)

// TestSocketE2E_GetNodeRoundTrip starts the real mcp.Server over the harness's
// fixture-indexed registry, dials the agent (mcp.sock) socket, and drives the
// full JSON-RPC protocol — initialize, tools/list, tools/call — asserting the
// socket round-trip reaches the registry and returns real fixture facts.
func TestSocketE2E_GetNodeRoundTrip(t *testing.T) {
	h := newHarness(t)

	mcpSock := startSocketServer(t, h.Registry())
	conn, err := net.DialTimeout("unix", mcpSock, time.Second)
	if err != nil {
		t.Fatalf("dial mcp.sock: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	rd := bufio.NewReader(conn)

	// (a) initialize → non-error response.
	initResp := socketRoundTrip(t, conn, rd, 1, "initialize", nil)
	if initResp.Error != nil {
		t.Fatalf("initialize returned error: %+v", initResp.Error)
	}
	if initResp.Result == nil {
		t.Fatal("initialize returned nil result")
	}

	// (b) tools/list → catalog contains eng_get_node (and ~37 tools).
	listResp := socketRoundTrip(t, conn, rd, 2, "tools/list", nil)
	if listResp.Error != nil {
		t.Fatalf("tools/list returned error: %+v", listResp.Error)
	}
	names := toolNames(t, listResp.Result)
	if !containsName(names, "eng_get_node") {
		t.Fatalf("tools/list missing eng_get_node; got %d tools: %v", len(names), names)
	}
	if len(names) < 30 {
		t.Errorf("tools/list returned only %d tools, expected ~37", len(names))
	}
	t.Logf("tools/list returned %d tools", len(names))

	// (c) tools/call eng_get_node → result CONTAINS the resolved node id.
	nodeID := h.ResolveID(coverage.BetaRepoID, coverage.NodeKey{
		Path: "main.go", Kind: domain.KindFunction, Name: "main",
	})
	callParams := map[string]any{
		"name": "eng_get_node",
		"arguments": map[string]any{
			"node_id": string(nodeID),
			"repo_id": coverage.BetaRepoID,
		},
	}
	callResp := socketRoundTrip(t, conn, rd, 3, "tools/call", callParams)
	if callResp.Error != nil {
		t.Fatalf("tools/call eng_get_node returned error: %+v", callResp.Error)
	}
	resultJSON, err := json.Marshal(callResp.Result)
	if err != nil {
		t.Fatalf("re-marshal tools/call result: %v", err)
	}
	// Investigate the wrapper shape: handleToolsCall returns the tool result
	// raw (no MCP content-block wrapping), so the resolved id is a direct
	// substring. Substring match is robust either way.
	t.Logf("tools/call result shape: %s", truncate(string(resultJSON), 400))
	if !strings.Contains(string(resultJSON), string(nodeID)) {
		t.Fatalf("tools/call result does not contain resolved node id %q; result: %s",
			nodeID, string(resultJSON))
	}
}

// startSocketServer spins up a real mcp.Server over handler under t.TempDir
// sockets, waits for both socket files to appear, and returns the agent
// (mcp.sock) path. It mirrors server_test.go's startServer poll/cleanup pattern.
func startSocketServer(t *testing.T, handler mcp.Handler) (mcpSock string) {
	t.Helper()
	dir := t.TempDir()
	cliSock := filepath.Join(dir, "cli.sock")
	mcpSock = filepath.Join(dir, "mcp.sock")

	srv := mcp.NewServer(cliSock, mcpSock, handler)
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Start(ctx) }()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		_, e1 := os.Stat(cliSock)
		_, e2 := os.Stat(mcpSock)
		if e1 == nil && e2 == nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-errCh:
			if err != nil {
				t.Logf("server returned error on shutdown: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("server did not shut down in time")
		}
	})
	return mcpSock
}

// socketRoundTrip encodes a newline-terminated JSON-RPC request on conn and
// reads exactly one response line from rd, with a read deadline so a stall
// fails fast rather than hanging the suite.
func socketRoundTrip(
	t *testing.T, conn net.Conn, rd *bufio.Reader, id int, method string, params any,
) mcp.Response {
	t.Helper()
	req := mcp.Request{JSONRPC: "2.0", ID: rawID(id), Method: method}
	if params != nil {
		raw, err := json.Marshal(params)
		if err != nil {
			t.Fatalf("marshal params for %s: %v", method, err)
		}
		req.Params = raw
	}
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		t.Fatalf("encode %s request: %v", method, err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	line, err := rd.ReadBytes('\n')
	if err != nil {
		t.Fatalf("read %s response: %v", method, err)
	}
	var resp mcp.Response
	if err := json.Unmarshal(line, &resp); err != nil {
		t.Fatalf("decode %s response: %v", method, err)
	}
	return resp
}

// toolNames extracts the tool names from a tools/list result. The result was
// produced server-side as an mcp.ToolListResponse and round-tripped through
// JSON, so it decodes here as a generic object with a "tools" array.
func toolNames(t *testing.T, result any) []string {
	t.Helper()
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("re-marshal tools/list result: %v", err)
	}
	var parsed struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("decode tools/list result: %v", err)
	}
	names := make([]string, 0, len(parsed.Tools))
	for _, tool := range parsed.Tools {
		names = append(names, tool.Name)
	}
	return names
}

// rawID returns a *json.RawMessage carrying the JSON number n.
func rawID(n int) *json.RawMessage {
	b, _ := json.Marshal(n)
	raw := json.RawMessage(b)
	return &raw
}

func containsName(s []string, want string) bool {
	for _, v := range s {
		if v == want {
			return true
		}
	}
	return false
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
