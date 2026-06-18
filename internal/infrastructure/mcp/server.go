// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

// Package mcp provides a JSON-RPC 2.0 server over Unix domain sockets.
// Listeners are started for cli.sock (actor_kind=human) and mcp.sock (actor_kind=agent).
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"os/user"
	"sync"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

type Request struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Method  string           `json:"method"`
	Params  json.RawMessage  `json:"params,omitempty"`
}

type Response struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Result  any              `json:"result,omitempty"`
	Error   *RPCError        `json:"error,omitempty"`
}

type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603
)

const (
	CodeNotFound = -32002
)

// Handler defines the interface to process JSON-RPC requests with associated client actor attribution.
type Handler interface {
	Handle(ctx context.Context, actor domain.Actor, req *Request) (any, *RPCError)
}

// Server listens on two Unix sockets and dispatches JSON-RPC requests.
type Server struct {
	cliSock string
	mcpSock string
	handler Handler
}

// NewServer instantiates a server configured to listen on designated user and agent sockets.
func NewServer(cliSock, mcpSock string, handler Handler) *Server {
	return &Server{
		cliSock: cliSock,
		mcpSock: mcpSock,
		handler: handler,
	}
}

// Start spawns the listener routines and blocks until the context is cancelled, performing socket cleanup on shutdown.
func (s *Server) Start(ctx context.Context) error {
	cliL, err := listenUnix(s.cliSock)
	if err != nil {
		return err
	}
	mcpL, err := listenUnix(s.mcpSock)
	if err != nil {
		cliL.Close()
		os.Remove(s.cliSock)
		return err
	}

	go func() {
		<-ctx.Done()
		cliL.Close()
		mcpL.Close()
	}()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		s.acceptLoop(ctx, cliL, domain.ActorKindHuman)
	}()
	go func() {
		defer wg.Done()
		s.acceptLoop(ctx, mcpL, domain.ActorKindAgent)
	}()
	wg.Wait()

	os.Remove(s.cliSock)
	os.Remove(s.mcpSock)
	return nil
}

// listenUnix binds to a Unix socket path, ensuring stale files are removed first and permissions are locked to 0600.
func listenUnix(path string) (net.Listener, error) {
	os.Remove(path)
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0600); err != nil {
		l.Close()
		os.Remove(path)
		return nil, err
	}
	return l, nil
}

// humanActor resolves the calling OS user to attribute CLI actions to a specific human actor.
func humanActor() domain.Actor {
	u, err := user.Current()
	if err != nil || u.Username == "" {
		return domain.Actor{ID: "human:unknown", Kind: domain.ActorKindHuman}
	}
	return domain.Actor{ID: "human:" + u.Username, Kind: domain.ActorKindHuman}
}

func (s *Server) acceptLoop(ctx context.Context, l net.Listener, ak domain.ActorKind) {
	var wg sync.WaitGroup
	defer wg.Wait()

	for {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		wg.Add(1)
		go func(c net.Conn) {
			defer wg.Done()
			s.serveConn(ctx, c, ak)
		}(conn)
	}
}

type initializeParams struct {
	ClientInfo struct {
		Name string `json:"name"`
	} `json:"clientInfo"`
}

// serveConn processes a connection's lifetime, promoting agent client names during MCP initialization to audit agent activity.
func (s *Server) serveConn(ctx context.Context, conn net.Conn, ak domain.ActorKind) {
	defer conn.Close()

	var actor domain.Actor
	switch ak {
	case domain.ActorKindHuman:
		actor = humanActor()
	default:
		actor = domain.Actor{ID: "agent:unknown", Kind: domain.ActorKindAgent}
	}

	enc := json.NewEncoder(conn)
	scanner := bufio.NewScanner(conn)

	for scanner.Scan() {
		line := scanner.Bytes()

		var req Request
		if err := json.Unmarshal(line, &req); err != nil {
			// Parsing errors trigger immediate closure of the connection under the JSON-RPC spec.
			_ = enc.Encode(Response{
				JSONRPC: "2.0",
				Error:   &RPCError{Code: CodeParseError, Message: "parse error"},
			})
			return
		}

		if req.Method == "initialize" && actor.Kind == domain.ActorKindAgent {
			var p initializeParams
			if err := json.Unmarshal(req.Params, &p); err == nil && p.ClientInfo.Name != "" {
				actor.ID = "agent:" + p.ClientInfo.Name
			}
		}

		result, rpcErr := s.handler.Handle(ctx, actor, &req)

		// JSON-RPC 2.0 notifications carry no request identifier and must not receive a response.
		if req.ID == nil {
			continue
		}

		resp := Response{
			JSONRPC: "2.0",
			ID:      req.ID,
		}
		if rpcErr != nil {
			resp.Error = rpcErr
		} else {
			resp.Result = result
		}
		if err := enc.Encode(resp); err != nil {
			return
		}
	}
}
