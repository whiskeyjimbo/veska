// Package mcpclient is the CLI's JSON-RPC 2.0 client for the daemon's cli.sock.
// It owns the socket dialing (with cold-start retry), request framing, cwd
// injection, and CLI-flavored error humanization that the `veska` subcommands
// previously carried inline in cmd/veska (solov2-u4mv.5). It is a request/
// response client — distinct from internal/cli/mcp, the editor-facing stdio
// shim that proxies whole frames.
package mcpclient

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"

	"github.com/whiskeyjimbo/veska/internal/platform/config"
)

const (
	dialTimeout  = 2 * time.Second
	dialBackoff  = 200 * time.Millisecond
	dialAttempts = 3
	// ioTimeout is generous: the first call after `veska service start` (cold
	// daemon) can take ~10s as SQLite opens, the embedder hot-loads, and
	// registries initialise. 30s absorbs that cold-start jitter while staying
	// within human patience for a one-shot CLI call (solov2-d37i).
	ioTimeout = 30 * time.Second
)

// methodsSkipCwd lists eng_* methods that must NOT receive an auto-injected
// cwd: their CLI surface intentionally fans out across every registered repo
// when --repo is omitted (solov2-efzv), and pinning them to cwd would break the
// multi-repo workflow.
var methodsSkipCwd = map[string]struct{}{
	"eng_find_symbol":      {},
	"eng_get_context_pack": {},
}

// Call issues one JSON-RPC request to the daemon's cli.sock and decodes the
// result into out (which may be nil). It injects cwd into eng_* params, retries
// the dial across cold-start jitter, and returns CLI-humanized errors.
func Call(ctx context.Context, method string, params, out any) error {
	conn, err := dial(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(ioTimeout))

	if err := writeRequest(conn, method, injectCwd(method, params)); err != nil {
		return err
	}
	return readResponse(conn, out)
}

// IsDaemonUnreachable reports whether err is a dial failure (daemon down /
// socket absent) so callers can fall back to an in-process path.
func IsDaemonUnreachable(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "daemon not running") ||
		strings.Contains(s, "connection refused") ||
		strings.Contains(s, "no such file")
}

// dial opens cli.sock with a bounded retry. cli.sock (not mcp.sock) is required:
// the daemon classifies the actor by socket — cli.sock → human, mcp.sock →
// agent — and routing CLI commands through mcp.sock breaks human_required gates
// (solov2-7x7l).
func dial(ctx context.Context) (net.Conn, error) {
	sockPath := config.CLISockPath()
	var (
		conn    net.Conn
		dialErr error
		d       net.Dialer
	)
	d.Timeout = dialTimeout
	for attempt := range dialAttempts {
		conn, dialErr = d.DialContext(ctx, "unix", sockPath)
		if dialErr == nil {
			return conn, nil
		}
		if attempt < dialAttempts-1 {
			time.Sleep(dialBackoff)
		}
	}
	// Include the underlying cause (refused vs absent vs permission — solov2-0cg)
	// and, when the daemon simply isn't running, an actionable hint (solov2-j68l).
	es := dialErr.Error()
	if strings.Contains(es, "connection refused") ||
		strings.Contains(es, "no such file") ||
		strings.Contains(es, "no such file or directory") {
		return nil, fmt.Errorf("dial %s: daemon not running (start it with `veska service start`, or run `veska-daemon &` for a quick try): %w", sockPath, dialErr)
	}
	return nil, fmt.Errorf("dial %s after %d attempts: %w", sockPath, dialAttempts, dialErr)
}

func writeRequest(conn net.Conn, method string, params any) error {
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
	return nil
}

func readResponse(conn net.Conn, out any) error {
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
		return fmt.Errorf("daemon: %s", humanizeError(r.Error.Message))
	}
	if out != nil && len(r.Result) > 0 {
		if err := json.Unmarshal(r.Result, out); err != nil {
			return fmt.Errorf("decode result: %w", err)
		}
	}
	return nil
}

// injectCwd adds a "cwd" field to eng_* params so the daemon can resolve repo_id
// from the caller's working directory when omitted (solov2-ktz0), mirroring what
// veska-mcp does for editor clients. Non-eng_* methods, params already carrying
// cwd, and methodsSkipCwd entries pass through unchanged.
func injectCwd(method string, params any) any {
	if !strings.HasPrefix(method, "eng_") {
		return params
	}
	if _, skip := methodsSkipCwd[method]; skip {
		return params
	}
	cwd, err := os.Getwd()
	if err != nil || cwd == "" {
		return params
	}
	if params == nil {
		return map[string]any{"cwd": cwd}
	}
	m, ok := params.(map[string]any)
	if !ok {
		return params
	}
	if existing, _ := m["cwd"].(string); existing != "" {
		return m
	}
	m["cwd"] = cwd
	return m
}

// humanizeError rewrites MCP-protocol hints into CLI-flavored ones so `veska`
// users don't see eng_* tool names or JSON-RPC codes they can't act on
// (solov2-luc7).
func humanizeError(msg string) string {
	rep := strings.NewReplacer(
		"pass eng_list_repos to find the id", "run `veska repo list` to see ids",
		"run eng_list_repos", "run `veska repo list`",
	)
	return rep.Replace(msg)
}
