package mcp_test

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/mcp"
)

type stubActorHandler struct {
	mu     sync.Mutex
	actors []domain.Actor
	result any
	rpcErr *mcp.RPCError
}

func (h *stubActorHandler) Handle(_ context.Context, actor domain.Actor, req *mcp.Request) (any, *mcp.RPCError) {
	h.mu.Lock()
	h.actors = append(h.actors, actor)
	h.mu.Unlock()
	return h.result, h.rpcErr
}

func (h *stubActorHandler) lastActor() domain.Actor {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.actors) == 0 {
		return domain.Actor{}
	}
	return h.actors[len(h.actors)-1]
}

func mustRawJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("mustRawJSON: %v", err)
	}
	return b
}

func TestCliSock_ActorKindHuman_IDPrefixed(t *testing.T) {
	h := &stubActorHandler{result: map[string]string{"ok": "true"}}
	cliSock, _ := startServer(t, h)

	req := mcp.Request{JSONRPC: "2.0", ID: makeID(1), Method: "ping"}
	_ = sendRequest(t, cliSock, req)

	actor := h.lastActor()
	if actor.Kind != domain.ActorKindHuman {
		t.Errorf("actor.Kind = %q, want %q", actor.Kind, domain.ActorKindHuman)
	}
	if !strings.HasPrefix(actor.ID, "human:") {
		t.Errorf("actor.ID = %q, want prefix \"human:\"", actor.ID)
	}
}

func TestMcpSock_ActorKindAgent_DefaultID(t *testing.T) {
	h := &stubActorHandler{result: map[string]string{"ok": "true"}}
	_, mcpSock := startServer(t, h)

	req := mcp.Request{JSONRPC: "2.0", ID: makeID(1), Method: "ping"}
	_ = sendRequest(t, mcpSock, req)

	actor := h.lastActor()
	if actor.Kind != domain.ActorKindAgent {
		t.Errorf("actor.Kind = %q, want %q", actor.Kind, domain.ActorKindAgent)
	}
	if actor.ID != "agent:unknown" {
		t.Errorf("actor.ID = %q, want \"agent:unknown\"", actor.ID)
	}
}

func TestMcpSock_InitializeUpdatesActorID(t *testing.T) {
	h := &stubActorHandler{result: map[string]string{"ok": "true"}}
	_, mcpSock := startServer(t, h)

	conn, err := net.DialTimeout("unix", mcpSock, time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	enc := json.NewEncoder(conn)
	scanner := bufio.NewScanner(conn)

	initReq := mcp.Request{
		JSONRPC: "2.0",
		ID:      makeID(1),
		Method:  "initialize",
		Params:  mustRawJSON(t, map[string]any{"clientInfo": map[string]string{"name": "my-agent"}}),
	}
	if err := enc.Encode(initReq); err != nil {
		t.Fatalf("encode initialize: %v", err)
	}
	if !scanner.Scan() {
		t.Fatal("no response to initialize")
	}

	pingReq := mcp.Request{JSONRPC: "2.0", ID: makeID(2), Method: "ping"}
	if err := enc.Encode(pingReq); err != nil {
		t.Fatalf("encode ping: %v", err)
	}
	if !scanner.Scan() {
		t.Fatal("no response to ping")
	}

	actor := h.lastActor()
	if actor.Kind != domain.ActorKindAgent {
		t.Errorf("actor.Kind = %q, want %q", actor.Kind, domain.ActorKindAgent)
	}
	if actor.ID != "agent:my-agent" {
		t.Errorf("actor.ID = %q, want \"agent:my-agent\"", actor.ID)
	}
}
