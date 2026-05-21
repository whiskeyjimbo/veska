package main

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
	"github.com/whiskeyjimbo/veska/internal/config"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
	"github.com/whiskeyjimbo/veska/internal/repo"
)

// repoCmd returns the "repo" Cobra command with "add" and "remove" sub-commands.
// Both sub-commands prefer the running daemon's MCP socket (so they go through
// repoRegistrar.AddRepo / RemoveRepo and pick up the cold-scan + live-watch
// wiring) and fall back to a direct SQLite write when the daemon is unreachable.
func repoCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "repo",
		Short:        "Manage git repositories tracked by veska",
		SilenceUsage: true,
	}
	cmd.AddCommand(repoAddCmd())
	cmd.AddCommand(repoRemoveCmd())
	return cmd
}

func repoAddCmd() *cobra.Command {
	return &cobra.Command{
		Use:          "add <path>",
		Short:        "Register a git repository and install hooks",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			w := cmd.OutOrStdout()
			root := args[0]

			// Prefer the daemon when up — it triggers cold scan and seeds
			// the live watcher in one call (parity with eng_add_repo).
			id, dialErr := dialAddRepo(ctx, root)
			if dialErr == nil {
				fmt.Fprintf(w, "added repo %s (via daemon)\n", id)
				return nil
			}

			// Direct fallback: insert the row + install hooks. The next
			// daemon start will cold-scan it via StartupResync; live-watching
			// kicks in at that point too. Surface the dial error so the user
			// can tell 'daemon really is down' from 'daemon up but I can't
			// reach it' (solov2-0cg).
			db, closeFn, err := openLocalDB()
			if err != nil {
				return fmt.Errorf("repo add: %w", err)
			}
			defer closeFn()

			id, err = repo.Add(ctx, db, root)
			if err != nil {
				return fmt.Errorf("repo add: %w", err)
			}
			fmt.Fprintf(w, "added repo %s (direct write; daemon dial failed: %v — restart daemon to cold-scan/live-watch)\n", id, dialErr)
			return nil
		},
	}
}

func repoRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:          "remove <id>",
		Short:        "Deregister a repository and remove hooks",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			w := cmd.OutOrStdout()
			id := args[0]

			if err := dialRemoveRepo(ctx, id); err == nil {
				fmt.Fprintln(w, "removed (via daemon)")
				return nil
			}

			db, closeFn, err := openLocalDB()
			if err != nil {
				return fmt.Errorf("repo remove: %w", err)
			}
			defer closeFn()

			if err := repo.Remove(ctx, db, id); err != nil {
				return fmt.Errorf("repo remove: %w", err)
			}
			fmt.Fprintln(w, "removed (direct write; daemon offline)")
			return nil
		},
	}
}

// openLocalDB opens the on-disk sqlite database with full migrations applied
// and returns a close function so the caller releases the WAL connection
// promptly. Used as the fallback path when the daemon is not running.
func openLocalDB() (*sql.DB, func(), error) {
	dbPath := filepath.Join(config.DefaultVectorDir(), "veska.db")
	handle, err := sqlite.OpenWithOptions(dbPath, sqlite.Options{})
	if err != nil {
		return nil, nil, fmt.Errorf("open sqlite: %w", err)
	}
	return handle, func() { _ = handle.Close() }, nil
}

// dialAddRepo sends eng_add_repo over the daemon's MCP unix socket. Returns
// the assigned repo_id on success. Errors are translated into context — a
// dial failure means "daemon not running" and the caller should fall back.
func dialAddRepo(ctx context.Context, rootPath string) (string, error) {
	type result struct {
		RepoID      string `json:"repo_id"`
		ScanPending bool   `json:"scan_pending"`
	}
	var r result
	if err := callMCP(ctx, "eng_add_repo", map[string]any{"root_path": rootPath}, &r); err != nil {
		return "", err
	}
	if r.RepoID == "" {
		return "", errors.New("daemon returned empty repo_id")
	}
	return r.RepoID, nil
}

// dialRemoveRepo sends eng_remove_repo over the daemon's MCP unix socket.
func dialRemoveRepo(ctx context.Context, repoID string) error {
	var r struct{}
	return callMCP(ctx, "eng_remove_repo", map[string]any{"repo_id": repoID}, &r)
}

// callMCP performs a single newline-delimited JSON-RPC call against the
// daemon's MCP socket and decodes result into out. Returns an error if the
// dial fails (daemon not running), if the response is an error frame, or
// if decoding fails.
//
// The MCP server here speaks a direct flat protocol (method = tool name),
// not the standard MCP "tools/call" wrapper — see internal/infrastructure/mcp.
//
// Dialing retries 3× with 200ms backoff: a daemon restart binds the
// socket in two steps (listenUnix removes the stale path, then net.Listen
// creates a new one), and a CLI call racing that window used to fall
// straight through to the direct-write path even with the daemon up
// (solov2-0cg). 2s per-attempt + 3 attempts = ~6s ceiling, still well
// under any human-perceptible wait.
func callMCP(ctx context.Context, method string, params any, out any) error {
	const dialTimeout = 2 * time.Second
	const dialBackoff = 200 * time.Millisecond
	const dialAttempts = 3
	const ioTimeout = 5 * time.Second

	sockPath := config.MCPSockPath()
	var (
		conn    net.Conn
		dialErr error
		d       net.Dialer
	)
	d.Timeout = dialTimeout
	for attempt := 0; attempt < dialAttempts; attempt++ {
		conn, dialErr = d.DialContext(ctx, "unix", sockPath)
		if dialErr == nil {
			break
		}
		if attempt < dialAttempts-1 {
			time.Sleep(dialBackoff)
		}
	}
	if dialErr != nil {
		// Include the underlying dial error so 'daemon offline' messages
		// can tell the user what really happened (connection refused vs.
		// no such file vs. permission denied — solov2-0cg).
		return fmt.Errorf("dial %s after %d attempts: %w", sockPath, dialAttempts, dialErr)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(ioTimeout))

	type req struct {
		JSONRPC string `json:"jsonrpc"`
		ID      int    `json:"id"`
		Method  string `json:"method"`
		Params  any    `json:"params,omitempty"`
	}
	body, err := json.Marshal(req{JSONRPC: "2.0", ID: 1, Method: method, Params: params})
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}
	body = append(body, '\n')
	if _, err := conn.Write(body); err != nil {
		return fmt.Errorf("write request: %w", err)
	}
	if uc, ok := conn.(*net.UnixConn); ok {
		_ = uc.CloseWrite()
	}

	scanner := bufio.NewScanner(conn)
	// Allow large embedded results (e.g. find_symbol with many hits).
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
			return fmt.Errorf("read response: %w", err)
		}
		return errors.New("no response from daemon")
	}

	type rpcErr struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	type resp struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      int             `json:"id"`
		Result  json.RawMessage `json:"result,omitempty"`
		Error   *rpcErr         `json:"error,omitempty"`
	}
	var r resp
	if err := json.Unmarshal(scanner.Bytes(), &r); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	if r.Error != nil {
		return fmt.Errorf("daemon: %s (code %d)", r.Error.Message, r.Error.Code)
	}
	if out != nil && len(r.Result) > 0 {
		if err := json.Unmarshal(r.Result, out); err != nil {
			return fmt.Errorf("decode result: %w", err)
		}
	}
	return nil
}
