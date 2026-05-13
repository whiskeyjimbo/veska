package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"

	"github.com/spf13/cobra"
	"github.com/whiskeyjimbo/veska/internal/config"
)

const dialTimeout = 2 * time.Second

// ErrDaemonNotRunning is returned by runProxy when the MCP socket is not reachable.
var ErrDaemonNotRunning = errors.New("daemon not running")

// runProxy dials sockPath over Unix domain socket and bidirectionally proxies
// data between in/out and the socket until either side closes or ctx is cancelled.
// Returns ErrDaemonNotRunning (wrapped) when the socket is missing or refused.
func runProxy(ctx context.Context, sockPath string, in io.Reader, out io.Writer) error {
	dialCtx, dialCancel := context.WithTimeout(ctx, dialTimeout)
	defer dialCancel()

	var d net.Dialer
	rawConn, err := d.DialContext(dialCtx, "unix", sockPath)
	if err != nil {
		return fmt.Errorf("%w: %s", ErrDaemonNotRunning, sockPath)
	}
	defer rawConn.Close()

	unixConn, ok := rawConn.(*net.UnixConn)
	if !ok {
		// Fallback: treat as a plain conn without half-close.
		return proxyPlain(ctx, rawConn, in, out)
	}

	// Close the entire connection if ctx is cancelled while goroutines are running.
	proxyCtx, proxyCancel := context.WithCancel(ctx)
	defer proxyCancel()

	go func() {
		<-proxyCtx.Done()
		unixConn.Close()
	}()

	var wg sync.WaitGroup
	wg.Add(2)

	// stdin → socket: when in is exhausted, half-close the write side so the
	// server sees EOF without closing the read side of the connection.
	go func() {
		defer wg.Done()
		io.Copy(unixConn, in) //nolint:errcheck
		unixConn.CloseWrite() //nolint:errcheck
	}()

	// socket → stdout: when the server closes its end, cancel context to unblock
	// the other goroutine if it is still blocked.
	go func() {
		defer wg.Done()
		defer proxyCancel()
		io.Copy(out, unixConn) //nolint:errcheck
	}()

	wg.Wait()
	return nil
}

// proxyPlain is a fallback bidirectional copy for non-Unix connections.
// Either goroutine finishing cancels the other via context.
func proxyPlain(ctx context.Context, conn net.Conn, in io.Reader, out io.Writer) error {
	proxyCtx, proxyCancel := context.WithCancel(ctx)
	defer proxyCancel()

	go func() {
		<-proxyCtx.Done()
		conn.Close()
	}()

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		defer proxyCancel()
		io.Copy(conn, in) //nolint:errcheck
	}()

	go func() {
		defer wg.Done()
		defer proxyCancel()
		io.Copy(out, conn) //nolint:errcheck
	}()

	wg.Wait()
	return nil
}

// writeSocketMissingError writes a JSON-RPC 2.0 error to w when the daemon socket is missing.
// The error includes a data object with cli_command and socket_path fields so that
// editors speaking the MCP protocol receive a structured, actionable response.
func writeSocketMissingError(w io.Writer, sockPath string) {
	type errorData struct {
		CLICommand string `json:"cli_command"`
		SocketPath string `json:"socket_path"`
	}
	type errorBody struct {
		Code    int       `json:"code"`
		Message string    `json:"message"`
		Data    errorData `json:"data"`
	}
	type response struct {
		JSONRPC string    `json:"jsonrpc"`
		ID      any       `json:"id"`
		Error   errorBody `json:"error"`
	}

	resp := response{
		JSONRPC: "2.0",
		ID:      nil,
		Error: errorBody{
			Code:    -32000,
			Message: "daemon not running",
			Data: errorData{
				CLICommand: "veska service start",
				SocketPath: sockPath,
			},
		},
	}
	enc := json.NewEncoder(w)
	enc.Encode(resp) //nolint:errcheck
}

func newRootCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "veska-mcp",
		Short:        "Engram MCP stdio shim (proxies editor to daemon socket)",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			sockPath := config.MCPSockPath()
			err := runProxy(cmd.Context(), sockPath, os.Stdin, os.Stdout)
			if err != nil && errors.Is(err, ErrDaemonNotRunning) {
				writeSocketMissingError(os.Stdout, sockPath)
				fmt.Fprintln(os.Stderr, "veska-mcp: daemon not running. Start it with: veska service start")
				os.Exit(1)
			}
			return err
		},
	}
	return cmd
}

func main() {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
