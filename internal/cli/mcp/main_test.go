// SPDX-License-Identifier: AGPL-3.0-only

package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// TestSocketMissingJSONError verifies that writeSocketMissingError produces a
// JSON-RPC 2.0 error with the correct structure including cli_command and socket_path.
func TestSocketMissingJSONError(t *testing.T) {
	const sockPath = "/tmp/veska-test-nonexistent-12345.sock"
	var out bytes.Buffer
	writeSocketMissingError(&out, sockPath)

	var resp struct {
		JSONRPC string `json:"jsonrpc"`
		ID      any    `json:"id"`
		Error   struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
			Data    struct {
				CLICommand string `json:"cli_command"`
				SocketPath string `json:"socket_path"`
			} `json:"data"`
		} `json:"error"`
	}
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		t.Fatalf("stdout is not valid JSON: %v\nraw: %s", err, out.String())
	}
	if resp.JSONRPC != "2.0" {
		t.Errorf("jsonrpc = %q; want \"2.0\"", resp.JSONRPC)
	}
	if resp.ID != nil {
		t.Errorf("id = %v; want null", resp.ID)
	}
	if resp.Error.Code != -32000 {
		t.Errorf("error.code = %d; want -32000", resp.Error.Code)
	}
	if resp.Error.Message != "daemon not running" {
		t.Errorf("error.message = %q; want \"daemon not running\"", resp.Error.Message)
	}
	if resp.Error.Data.CLICommand != "veska service start" {
		t.Errorf("error.data.cli_command = %q; want \"veska service start\"", resp.Error.Data.CLICommand)
	}
	if resp.Error.Data.SocketPath != sockPath {
		t.Errorf("error.data.socket_path = %q; want %q", resp.Error.Data.SocketPath, sockPath)
	}
}

// TestRootCmdHelp verifies that --help exits without error.
func TestRootCmdHelp(t *testing.T) {
	root := NewCmd()
	root.SetArgs([]string{"--help"})
	if err := root.Execute(); err != nil {
		t.Fatalf("expected nil error from --help, got: %v", err)
	}
}

// TestRunProxy_MissingSocket verifies that runProxy returns an error containing
// "daemon not running" when the socket path does not exist.
func TestRunProxy_MissingSocket(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	err := runProxy(ctx, "/tmp/veska-mcp-test-nonexistent.sock", strings.NewReader(""), io.Discard)
	if err == nil {
		t.Fatal("expected an error from runProxy with missing socket, got nil")
	}
	if !strings.Contains(err.Error(), "daemon not running") {
		t.Errorf("error = %q; want it to contain \"daemon not running\"", err.Error())
	}
}

// TestRunProxy_ProxiesData verifies that bytes written to the reader arrive at
// the mock Unix socket server and that bytes sent back from the server arrive at
// the writer.
func TestRunProxy_ProxiesData(t *testing.T) {
	// Create a temp Unix socket listener.
	tmp := t.TempDir()
	sockPath := tmp + "/mcp.sock"

	l, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("failed to listen on test socket: %v", err)
	}
	defer l.Close()

	clientMsg := "hello from client\n"
	serverMsg := "hello from server\n"

	// Server: read what the client sends, echo a fixed response, close.
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		conn, err := l.Accept()
		if err != nil {
			return
		}
		defer conn.Close()

		// Read client bytes.
		buf := make([]byte, len(clientMsg))
		if _, err := io.ReadFull(conn, buf); err != nil {
			return
		}
		// Write server bytes.
		conn.Write([]byte(serverMsg)) //nolint:errcheck
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	in := strings.NewReader(clientMsg)
	var out bytes.Buffer

	if err := runProxy(ctx, sockPath, in, &out); err != nil {
		t.Fatalf("runProxy returned unexpected error: %v", err)
	}

	// Verify server received client bytes (indirectly: server closed after read).
	<-serverDone

	// Verify client received server bytes.
	if got := out.String(); got != serverMsg {
		t.Errorf("out = %q; want %q", got, serverMsg)
	}
}
