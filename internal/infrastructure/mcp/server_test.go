package mcp_test

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/mcp"
)

// stubHandler records the actor of each call and returns a fixed result or error.
type stubHandler struct {
	mu     sync.Mutex
	calls  []domain.Actor
	result any
	rpcErr *mcp.RPCError
}

func (h *stubHandler) Handle(_ context.Context, actor domain.Actor, req *mcp.Request) (any, *mcp.RPCError) {
	h.mu.Lock()
	h.calls = append(h.calls, actor)
	h.mu.Unlock()
	return h.result, h.rpcErr
}

func (h *stubHandler) lastKind() domain.ActorKind {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.calls) == 0 {
		return ""
	}
	return h.calls[len(h.calls)-1].Kind
}

// startServer spins up a Server and waits until both sockets exist.
func startServer(t *testing.T, handler mcp.Handler) (cliSock, mcpSock string) {
	t.Helper()
	dir := t.TempDir()
	cliSock = filepath.Join(dir, "cli.sock")
	mcpSock = filepath.Join(dir, "mcp.sock")

	srv := mcp.NewServer(cliSock, mcpSock, handler)
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Start(ctx) }()

	// wait for sockets to appear (up to 2 s)
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
	return cliSock, mcpSock
}

// sendRequest dials path, sends req as newline-terminated JSON, reads one response line.
func sendRequest(t *testing.T, path string, req mcp.Request) mcp.Response {
	t.Helper()
	conn, err := net.DialTimeout("unix", path, time.Second)
	if err != nil {
		t.Fatalf("dial %s: %v", path, err)
	}
	defer conn.Close()

	if err := json.NewEncoder(conn).Encode(req); err != nil {
		t.Fatalf("encode request: %v", err)
	}

	var resp mcp.Response
	scanner := bufio.NewScanner(conn)
	if !scanner.Scan() {
		t.Fatalf("no response received from %s", path)
	}
	if err := json.Unmarshal(scanner.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return resp
}

// makeID returns a *json.RawMessage containing the JSON number n.
func makeID(n int) *json.RawMessage {
	b := json.RawMessage([]byte{byte('0' + n)})
	return &b
}

func TestCliSockActorKindHuman(t *testing.T) {
	h := &stubHandler{result: map[string]string{"ok": "true"}}
	cliSock, _ := startServer(t, h)

	req := mcp.Request{JSONRPC: "2.0", ID: makeID(1), Method: "ping"}
	_ = sendRequest(t, cliSock, req)

	if got := h.lastKind(); got != domain.ActorKindHuman {
		t.Errorf("actorKind = %q, want %q", got, domain.ActorKindHuman)
	}
}

func TestMcpSockActorKindAgent(t *testing.T) {
	h := &stubHandler{result: map[string]string{"ok": "true"}}
	_, mcpSock := startServer(t, h)

	req := mcp.Request{JSONRPC: "2.0", ID: makeID(1), Method: "ping"}
	_ = sendRequest(t, mcpSock, req)

	if got := h.lastKind(); got != domain.ActorKindAgent {
		t.Errorf("actorKind = %q, want %q", got, domain.ActorKindAgent)
	}
}

func TestValidRequestResponse(t *testing.T) {
	result := map[string]int{"value": 42}
	h := &stubHandler{result: result}
	cliSock, _ := startServer(t, h)

	id := makeID(7)
	req := mcp.Request{JSONRPC: "2.0", ID: id, Method: "echo"}
	resp := sendRequest(t, cliSock, req)

	if resp.JSONRPC != "2.0" {
		t.Errorf("jsonrpc = %q, want 2.0", resp.JSONRPC)
	}
	if resp.Error != nil {
		t.Errorf("unexpected error: %+v", resp.Error)
	}
	if resp.Result == nil {
		t.Error("expected non-nil result")
	}
	// ID should be echoed back
	if resp.ID == nil {
		t.Error("expected response id to be set")
	}
}

func TestMalformedJSONParseError(t *testing.T) {
	h := &stubHandler{result: map[string]string{"ok": "true"}}
	cliSock, _ := startServer(t, h)

	conn, err := net.DialTimeout("unix", cliSock, time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// send malformed JSON terminated by newline
	if _, err := conn.Write([]byte("{bad json}\n")); err != nil {
		t.Fatalf("write: %v", err)
	}

	var resp mcp.Response
	scanner := bufio.NewScanner(conn)
	if !scanner.Scan() {
		t.Fatal("no response received")
	}
	if err := json.Unmarshal(scanner.Bytes(), &resp); err != nil {
		t.Fatalf("decode parse-error response: %v", err)
	}
	if resp.Error == nil {
		t.Fatal("expected error in response")
		return
	}
	if resp.Error.Code != mcp.CodeParseError {
		t.Errorf("error code = %d, want %d", resp.Error.Code, mcp.CodeParseError)
	}
}

func TestHandlerRPCErrorForwarded(t *testing.T) {
	rpcErr := &mcp.RPCError{Code: mcp.CodeMethodNotFound, Message: "no such method"}
	h := &stubHandler{rpcErr: rpcErr}
	cliSock, _ := startServer(t, h)

	req := mcp.Request{JSONRPC: "2.0", ID: makeID(3), Method: "unknown"}
	resp := sendRequest(t, cliSock, req)

	if resp.Error == nil {
		t.Fatal("expected error response")
		return
	}
	if resp.Error.Code != mcp.CodeMethodNotFound {
		t.Errorf("error code = %d, want %d", resp.Error.Code, mcp.CodeMethodNotFound)
	}
	if resp.Error.Message != "no such method" {
		t.Errorf("error message = %q, want %q", resp.Error.Message, "no such method")
	}
	if resp.Result != nil {
		t.Error("result should be nil when error is set")
	}
}

func TestCtxCancelShutdown(t *testing.T) {
	h := &stubHandler{result: map[string]string{"ok": "true"}}
	dir := t.TempDir()
	cliSock := filepath.Join(dir, "cli.sock")
	mcpSock := filepath.Join(dir, "mcp.sock")

	srv := mcp.NewServer(cliSock, mcpSock, h)
	ctx, cancel := context.WithCancel(context.Background())

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Start(ctx) }()

	// wait for sockets
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		_, e1 := os.Stat(cliSock)
		_, e2 := os.Stat(mcpSock)
		if e1 == nil && e2 == nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	// cancel and expect clean shutdown
	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("server returned error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Error("server did not shut down within 3 seconds after ctx cancel")
	}

	// socket files should be cleaned up
	if _, err := os.Stat(cliSock); !os.IsNotExist(err) {
		t.Error("cli.sock was not removed after shutdown")
	}
	if _, err := os.Stat(mcpSock); !os.IsNotExist(err) {
		t.Error("mcp.sock was not removed after shutdown")
	}
}
