package mcp

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
	"github.com/whiskeyjimbo/veska/internal/platform/config"
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
		return proxyPlain(ctx, rawConn, in, out)
	}

	proxyCtx, proxyCancel := context.WithCancel(ctx)
	defer proxyCancel()

	go func() {
		<-proxyCtx.Done()
		unixConn.Close()
	}()

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		injectCwdAndCopy(unixConn, in)
		unixConn.CloseWrite() //nolint:errcheck
	}()

	go func() {
		defer wg.Done()
		defer proxyCancel()
		io.Copy(out, unixConn) //nolint:errcheck
	}()

	wg.Wait()
	return nil
}

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

// NewCmd returns the mcp cobra command, suitable for AddCommand under the
// veska root or for direct Execute via Run.
func NewCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "Veska MCP stdio shim (proxies editor to daemon socket)",
		Long: `veska mcp is the editor-facing shim. It reads newline-delimited
JSON-RPC requests on stdin, forwards them to the daemon's MCP socket
($VESKA_HOME/mcp.sock), and writes responses back to stdout.

Protocol: flat JSON-RPC. The method IS the tool name; there is no
"tools/call" envelope. Parameters go in the standard "params" field.

Example (from a shell, with the daemon running):

  printf '{"jsonrpc":"2.0","id":1,"method":"eng_get_status","params":{}}\n' \
    | veska-mcp

Editor integration: point your MCP client at this binary as a stdio
command. Examples for Claude Desktop / Cursor / Continue / Zed live in
README.md → "Editor integration".`,
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

// Run is the entry point used when the binary is invoked as veska-mcp via
// the argv[0] dispatcher in cmd/veska/main.go.
func Run() int {
	cmd := NewCmd()
	cmd.Use = "veska-mcp"
	if err := cmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}
