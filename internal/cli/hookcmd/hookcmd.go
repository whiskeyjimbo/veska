// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

// Package hookcmd holds the business logic behind the git-hook shims that
// `veska init` installs (`veska hook-runner post-commit` and
// `hook-runner post-checkout`). cmd/veska/hook_runner.go is reduced to Cobra
// command construction whose RunE bodies delegate here, following the
// cmd = glue / logic-in-packages pattern established by reindexcmd, symbolcmd,
// graphcmd, and findingscmd.
// Every entry point swallows its errors and returns nil: a git hook that exits
// non-zero blocks the user's commit or checkout, which is never an acceptable
// failure mode for a best-effort index notification.
package hookcmd

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/whiskeyjimbo/veska/internal/infrastructure/repo"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite/sqldriver"
	"github.com/whiskeyjimbo/veska/internal/platform/config"
)

// RunPostCommit is the top-level entry point for the post-commit hook. It
// always returns nil so that git never sees a non-zero exit and never blocks a
// commit.
func RunPostCommit() error {
	gitRoot, err := gitRevParseTopLevel()
	if err != nil {
		// Not in a git repo or git unavailable - silently succeed.
		debugf("hook-runner: git rev-parse failed: %v\n", err)
		return nil
	}

	if IsGitSpecialState(gitRoot) {
		debugf("hook-runner: git special state detected, skipping\n")
		return nil
	}

	// Belt-and-braces: try the VESKA_HOME-derived socket
	// first, then fall back to ~/.veska/cli.sock so a stale baked
	// VESKA_HOME in the hook script (or an unset env) still finds a
	// running daemon on the default path.
	candidates := []string{config.CLISockPath()}
	if home, err := os.UserHomeDir(); err == nil {
		def := filepath.Join(home, ".veska", "cli.sock")
		if def != candidates[0] {
			candidates = append(candidates, def)
		}
	}
	for _, sockPath := range candidates {
		if err := SendSeal(sockPath); err != nil {
			debugf("hook-runner: sendSeal %s (ignored): %v\n", sockPath, err)
			continue
		}
		debugf("hook-runner: sendSeal %s OK\n", sockPath)
		return nil
	}
	return nil
}

// RunPostCheckout is the top-level entry point for the post-checkout hook. It
// always returns nil so that git never sees a non-zero exit and never blocks a
// checkout.
func RunPostCheckout() error {
	gitRoot, err := gitRevParseTopLevel()
	if err != nil {
		debugf("hook-runner post-checkout: git rev-parse failed: %v\n", err)
		return nil
	}

	if IsGitSpecialState(gitRoot) {
		debugf("hook-runner post-checkout: git special state detected, skipping\n")
		return nil
	}

	branch, err := gitCurrentBranch()
	if err != nil {
		debugf("hook-runner post-checkout: could not determine branch: %v\n", err)
		return nil
	}

	// repo.RepoIDForPath canonicalises (absolute + symlink-resolved) before
	// hashing, exactly as registration does, so the id matches the row the
	// registry stored even when the checkout is reached through a symlinked
	// path (the previous local sha256 hashed the raw rev-parse output
	// and could silently miss every row).
	repoID := repo.RepoIDForPath(gitRoot)

	dbPath := filepath.Join(config.DefaultVectorDir(), "veska.db")
	db, err := sql.Open(sqldriver.Name, dbPath)
	if err != nil {
		debugf("hook-runner post-checkout: open db: %v\n", err)
		return nil
	}
	defer db.Close()

	if err := repo.SetActiveBranch(context.Background(), db, repoID, branch); err != nil {
		debugf("hook-runner post-checkout: SetActiveBranch: %v\n", err)
	}

	sockPath := config.CLISockPath()
	if err := SendSeal(sockPath); err != nil {
		debugf("hook-runner post-checkout: sendSeal error (ignored): %v\n", err)
	}
	return nil
}

// IsGitSpecialState reports whether the working tree is in a merge, rebase,
// cherry-pick, or bisect state. gitRoot must be the absolute path returned by
// git rev-parse --show-toplevel.
func IsGitSpecialState(gitRoot string) bool {
	gitDir := filepath.Join(gitRoot, ".git")

	// Files that indicate a special state.
	specialFiles := []string{
		filepath.Join(gitDir, "MERGE_HEAD"),
		filepath.Join(gitDir, "CHERRY_PICK_HEAD"),
		filepath.Join(gitDir, "BISECT_LOG"),
	}
	for _, f := range specialFiles {
		if _, err := os.Stat(f); err == nil {
			return true
		}
	}

	// Directories that indicate a rebase is in progress.
	specialDirs := []string{
		filepath.Join(gitDir, "rebase-merge"),
		filepath.Join(gitDir, "rebase-apply"),
	}
	for _, d := range specialDirs {
		if info, err := os.Stat(d); err == nil && info.IsDir() {
			return true
		}
	}

	return false
}

// gitRevParseTopLevel returns the absolute path of the git work tree root.
func gitRevParseTopLevel() (string, error) {
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// gitCurrentBranch returns the current branch name via git rev-parse.
func gitCurrentBranch() (string, error) {
	out, err := exec.Command("git", "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// SendSeal dials the daemon CLI socket and invokes the eng_promote MCP tool
// for the current git working tree. The daemon re-stages files changed in
// HEAD and promotes. All errors after a successful dial are silently swallowed
// the hook must never block a commit (git would surface a non-zero exit to
// the user). A dial failure is returned so the caller can fall back to the
// next candidate socket.
// this used to send a legacy {"cmd":"promote"} payload that the
// JSON-RPC listener rejected with method-not-found, so post-commit
// promotion was silently dead. Now it speaks the same JSON-RPC the rest of
// the MCP surface does.
func SendSeal(sockPath string) error {
	const dialTimeout = 250 * time.Millisecond
	const ioTimeout = 5 * time.Second

	gitRoot, err := gitRevParseTopLevel()
	if err != nil {
		debugf("hook-runner: git rev-parse failed: %v\n", err)
		return nil
	}

	conn, err := net.DialTimeout("unix", sockPath, dialTimeout)
	if err != nil {
		// Dial failure is the signal the caller uses to try the next
		// candidate socket. All other errors after a
		// successful dial are still swallowed so a misbehaving daemon
		// cannot block git.
		return err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(ioTimeout))

	type rpcReq struct {
		JSONRPC string `json:"jsonrpc"`
		ID      int    `json:"id"`
		Method  string `json:"method"`
		Params  any    `json:"params,omitempty"`
	}
	body, err := json.Marshal(rpcReq{
		JSONRPC: "2.0", ID: 1, Method: "eng_promote_repo",
		Params: map[string]string{"root_path": gitRoot},
	})
	if err != nil {
		return nil
	}
	body = append(body, '\n')
	if _, err := conn.Write(body); err != nil {
		debugf("hook-runner: write: %v\n", err)
		return nil
	}
	if uc, ok := conn.(*net.UnixConn); ok {
		_ = uc.CloseWrite()
	}

	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 0, 8*1024), 64*1024)
	if scanner.Scan() {
		debugf("hook-runner: eng_promote response: %s\n", scanner.Text())
	}
	return nil
}

// debugf logs to stderr only when VESKA_DEBUG=1 is set.
func debugf(format string, args ...any) {
	if os.Getenv("VESKA_DEBUG") == "1" {
		fmt.Fprintf(os.Stderr, format, args...)
	}
}
