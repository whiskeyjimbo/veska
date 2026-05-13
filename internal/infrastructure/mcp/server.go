// Package mcp provides a JSON-RPC 2.0 server over Unix domain sockets.
// Two listeners are started: cli.sock (actor_kind=human) and mcp.sock (actor_kind=agent).
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"sync"

	"github.com/whiskeyjimbo/engram/solov2/internal/core/domain"
)

// Request is an inbound JSON-RPC 2.0 frame.
type Request struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Method  string           `json:"method"`
	Params  json.RawMessage  `json:"params,omitempty"`
}

// Response is an outbound JSON-RPC 2.0 frame.
type Response struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Result  any              `json:"result,omitempty"`
	Error   *RPCError        `json:"error,omitempty"`
}

// RPCError is the JSON-RPC error object.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Standard JSON-RPC 2.0 error codes.
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603
)

// Handler processes one JSON-RPC request and returns a result or error.
// actorKind indicates whether the caller is human (cli.sock) or agent (mcp.sock).
type Handler interface {
	Handle(ctx context.Context, actorKind domain.ActorKind, req *Request) (any, *RPCError)
}

// Server listens on two Unix sockets and dispatches JSON-RPC requests.
type Server struct {
	cliSock string
	mcpSock string
	handler Handler
}

// NewServer creates a server that will listen on cliSock (actor=human) and
// mcpSock (actor=agent). Both sockets are created with mode 0600.
func NewServer(cliSock, mcpSock string, handler Handler) *Server {
	return &Server{
		cliSock: cliSock,
		mcpSock: mcpSock,
		handler: handler,
	}
}

// Start creates the socket files, begins accepting connections, and serves
// until ctx is cancelled. Returns when both listeners have shut down.
// Cleans up socket files on exit.
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

	// When ctx is cancelled, close both listeners to unblock Accept.
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

	// Clean up socket files.
	os.Remove(s.cliSock)
	os.Remove(s.mcpSock)
	return nil
}

// listenUnix removes any stale socket, creates a new listener, and chmods it to 0600.
func listenUnix(path string) (net.Listener, error) {
	os.Remove(path) // ignore error — file may not exist
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

// acceptLoop accepts connections on l until the listener is closed (ctx cancelled).
func (s *Server) acceptLoop(ctx context.Context, l net.Listener, ak domain.ActorKind) {
	var wg sync.WaitGroup
	defer wg.Wait()

	for {
		conn, err := l.Accept()
		if err != nil {
			// Listener closed — normal shutdown path.
			return
		}
		wg.Add(1)
		go func(c net.Conn) {
			defer wg.Done()
			s.serveConn(ctx, c, ak)
		}(conn)
	}
}

// serveConn handles one client connection: read newline-delimited JSON requests,
// dispatch to the handler, write JSON-RPC responses.
func (s *Server) serveConn(ctx context.Context, conn net.Conn, ak domain.ActorKind) {
	defer conn.Close()

	enc := json.NewEncoder(conn)
	scanner := bufio.NewScanner(conn)

	for scanner.Scan() {
		line := scanner.Bytes()

		var req Request
		if err := json.Unmarshal(line, &req); err != nil {
			// Parse error — send -32700, then close connection per spec.
			_ = enc.Encode(Response{
				JSONRPC: "2.0",
				Error:   &RPCError{Code: CodeParseError, Message: "parse error"},
			})
			return
		}

		result, rpcErr := s.handler.Handle(ctx, ak, &req)

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
	// scanner.Err() == nil means EOF (client closed connection) — exit cleanly.
}
