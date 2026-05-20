package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/whiskeyjimbo/veska/internal/config"
	"github.com/whiskeyjimbo/veska/internal/repo"

	_ "modernc.org/sqlite"
)

// hookRunnerCmd returns the "hook-runner" Cobra command with sub-commands.
func hookRunnerCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "hook-runner",
		Short:        "Git hook shims installed by veska init",
		SilenceUsage: true,
	}
	cmd.AddCommand(postCommitCmd())
	cmd.AddCommand(postCheckoutCmd())
	return cmd
}

// postCommitCmd returns the "hook-runner post-commit" sub-command.
func postCommitCmd() *cobra.Command {
	return &cobra.Command{
		Use:          "post-commit",
		Short:        "Notify daemon after a git commit (installed by veska init)",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPostCommit()
		},
	}
}

// runPostCommit is the top-level entry point for the hook. It always returns
// nil so that git never sees a non-zero exit and never blocks a commit.
func runPostCommit() error {
	gitRoot, err := gitRevParseTopLevel()
	if err != nil {
		// Not in a git repo or git unavailable — silently succeed.
		debugf("hook-runner: git rev-parse failed: %v\n", err)
		return nil
	}

	if isGitSpecialState(gitRoot) {
		debugf("hook-runner: git special state detected, skipping\n")
		return nil
	}

	// Belt-and-braces (solov2-g50): try the VESKA_HOME-derived socket
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
		if err := sendSeal(sockPath); err != nil {
			debugf("hook-runner: sendSeal %s (ignored): %v\n", sockPath, err)
			continue
		}
		debugf("hook-runner: sendSeal %s OK\n", sockPath)
		return nil
	}
	return nil
}

// isGitSpecialState reports whether the working tree is in a merge, rebase,
// cherry-pick, or bisect state. gitRoot must be the absolute path returned by
// git rev-parse --show-toplevel.
func isGitSpecialState(gitRoot string) bool {
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

// sendSeal dials the daemon CLI socket and invokes the eng_promote MCP tool
// for the current git working tree. The daemon re-stages files changed in
// HEAD and promotes. All errors are silently swallowed — the hook must
// never block a commit (git would surface a non-zero exit to the user).
//
// Solov2-3vv: this used to send a legacy {"cmd":"promote"} payload that the
// JSON-RPC listener rejected with method-not-found, so post-commit
// promotion was silently dead. Now it speaks the same JSON-RPC the rest of
// the MCP surface does.
func sendSeal(sockPath string) error {
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
		// candidate socket (solov2-g50). All other errors after a
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

// postCheckoutCmd returns the "hook-runner post-checkout" sub-command.
func postCheckoutCmd() *cobra.Command {
	return &cobra.Command{
		Use:          "post-checkout",
		Short:        "Update active branch after a git checkout (installed by veska init)",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPostCheckout()
		},
	}
}

// runPostCheckout is the top-level entry point for the post-checkout hook. It
// always returns nil so that git never sees a non-zero exit and never blocks a
// checkout.
func runPostCheckout() error {
	gitRoot, err := gitRevParseTopLevel()
	if err != nil {
		debugf("hook-runner post-checkout: git rev-parse failed: %v\n", err)
		return nil
	}

	if isGitSpecialState(gitRoot) {
		debugf("hook-runner post-checkout: git special state detected, skipping\n")
		return nil
	}

	branch, err := gitCurrentBranch()
	if err != nil {
		debugf("hook-runner post-checkout: could not determine branch: %v\n", err)
		return nil
	}

	repoID := repoIDFromPath(gitRoot)

	dbPath := filepath.Join(config.DefaultVectorDir(), "veska.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		debugf("hook-runner post-checkout: open db: %v\n", err)
		return nil
	}
	defer db.Close()

	if err := repo.SetActiveBranch(context.Background(), db, repoID, branch); err != nil {
		debugf("hook-runner post-checkout: SetActiveBranch: %v\n", err)
	}

	sockPath := config.CLISockPath()
	if err := sendSeal(sockPath); err != nil {
		debugf("hook-runner post-checkout: sendSeal error (ignored): %v\n", err)
	}
	return nil
}

// gitCurrentBranch returns the current branch name via git rev-parse.
func gitCurrentBranch() (string, error) {
	out, err := exec.Command("git", "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// repoIDFromPath returns the deterministic sha256 hex ID for a canonical path.
// This mirrors the repoID computation in the repo package.
func repoIDFromPath(canonicalPath string) string {
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "%s", canonicalPath)
	return hex.EncodeToString(h.Sum(nil))
}
