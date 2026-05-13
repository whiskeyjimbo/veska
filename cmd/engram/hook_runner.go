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
	"github.com/whiskeyjimbo/engram/solov2/internal/config"
	"github.com/whiskeyjimbo/engram/solov2/internal/repo"

	_ "modernc.org/sqlite"
)

// hookRunnerCmd returns the "hook-runner" Cobra command with sub-commands.
func hookRunnerCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "hook-runner",
		Short:        "Git hook shims installed by engram init",
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
		Short:        "Notify daemon after a git commit (installed by engram init)",
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

	sockPath := config.DaemonSockPath()
	if err := sendSeal(sockPath); err != nil {
		debugf("hook-runner: sendSeal error (ignored): %v\n", err)
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

// sealMessage is the JSON payload sent to the daemon.
type sealMessage struct {
	Cmd string `json:"cmd"`
}

// sealResponse is the expected JSON response from the daemon.
type sealResponse struct {
	OK bool `json:"ok"`
}

// sendSeal dials the daemon Unix socket, sends {"cmd":"promote"}, and reads
// the response. All errors are silently swallowed — the hook must never block
// a commit.
func sendSeal(sockPath string) error {
	const dialTimeout = 50 * time.Millisecond
	const readTimeout = 50 * time.Millisecond

	conn, err := net.DialTimeout("unix", sockPath, dialTimeout)
	if err != nil {
		// Socket unavailable — daemon not running. This is expected.
		debugf("hook-runner: dial %s: %v\n", sockPath, err)
		return nil
	}
	defer conn.Close()

	msg := sealMessage{Cmd: "promote"}
	payload, err := json.Marshal(msg)
	if err != nil {
		return nil // should never happen
	}
	payload = append(payload, '\n')

	if _, err := conn.Write(payload); err != nil {
		debugf("hook-runner: write: %v\n", err)
		return nil
	}

	// Signal EOF on the write side so the daemon knows the message is complete.
	if tc, ok := conn.(*net.UnixConn); ok {
		tc.CloseWrite() //nolint:errcheck
	}

	// Read response with deadline.
	if err := conn.SetReadDeadline(time.Now().Add(readTimeout)); err != nil {
		return nil
	}

	scanner := bufio.NewScanner(conn)
	if scanner.Scan() {
		var resp sealResponse
		if err := json.Unmarshal(scanner.Bytes(), &resp); err != nil {
			debugf("hook-runner: parse response: %v\n", err)
		}
		// resp.OK == true is the happy path; anything else is still ok — we exit 0.
	}
	// Read timeout or EOF — both are fine.
	return nil
}

// debugf logs to stderr only when ENGRAM_DEBUG=1 is set.
func debugf(format string, args ...any) {
	if os.Getenv("ENGRAM_DEBUG") == "1" {
		fmt.Fprintf(os.Stderr, format, args...)
	}
}

// postCheckoutCmd returns the "hook-runner post-checkout" sub-command.
func postCheckoutCmd() *cobra.Command {
	return &cobra.Command{
		Use:          "post-checkout",
		Short:        "Update active branch after a git checkout (installed by engram init)",
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

	dbPath := filepath.Join(config.DefaultVectorDir(), "engram.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		debugf("hook-runner post-checkout: open db: %v\n", err)
		return nil
	}
	defer db.Close()

	if err := repo.SetActiveBranch(context.Background(), db, repoID, branch); err != nil {
		debugf("hook-runner post-checkout: SetActiveBranch: %v\n", err)
	}

	sockPath := config.DaemonSockPath()
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
