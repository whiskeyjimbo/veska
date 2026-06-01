// Package configcmd holds the business logic behind the `veska config` command
// family. cmd/veska/config.go is reduced to Cobra construction whose RunE bodies
// delegate here, following the cmd = glue / logic-in-packages pattern
// established by symbolcmd, depscmd, findingscmd, and doctorcmd .
package configcmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/whiskeyjimbo/veska/internal/cli/mcpclient"
	"github.com/whiskeyjimbo/veska/internal/platform/service"
)

// ErrNoManager is returned by RunReload when no ServiceManager was wired in
// (e.g. during early bootstrap before os.Executable resolves the daemon path).
var ErrNoManager = errors.New("service manager not available")

// CallFunc issues one MCP request against the daemon. It mirrors
// mcpclient.Call so RunReload can be unit-tested with a fake in place of the
// real socket client.
type CallFunc func(ctx context.Context, method string, params, out any) error

// ReloadParams bundles the inputs of RunReload.
type ReloadParams struct {
	// Manager mutates supervisor state; nil yields ErrNoManager.
	Manager service.Manager
	Out     io.Writer
	// DaemonReady reports whether the daemon socket is back up after a
	// restart. Injected (cmd/veska's daemonRunning) so the package needs no
	// socket-dialing dependency; required.
	DaemonReady func() bool
	// Call defaults to mcpclient.Call when nil.
	Call CallFunc
	// PollTimeout/PollInterval bound the wait for the daemon to come back.
	// Zero values fall back to the production defaults; tests shrink them.
	PollTimeout  time.Duration
	PollInterval time.Duration
}

// RunReload restarts the daemon so it re-reads ~/.veska/config.toml, waits for
// it to come back up, then re-promotes every registered repo so check rules
// added by the new config (notably vuln-scan when [vuln_source] is on) surface
// findings on already-promoted code. Without it, a config change is a three-step
// chore: service stop -> service start -> reindex <path> for every repo.
func RunReload(ctx context.Context, p ReloadParams) error {
	if p.Manager == nil {
		return ErrNoManager
	}
	call := p.Call
	if call == nil {
		call = mcpclient.Call
	}
	pollTimeout := p.PollTimeout
	if pollTimeout == 0 {
		pollTimeout = 15 * time.Second
	}
	pollInterval := p.PollInterval
	if pollInterval == 0 {
		pollInterval = 250 * time.Millisecond
	}

	// 1) Restart so the daemon re-reads ~/.veska/config.toml.
	fmt.Fprintln(p.Out, "restarting daemon to pick up config changes...")
	if err := p.Manager.Restart(ctx); err != nil {
		return fmt.Errorf("config reload: restart: %w", err)
	}

	// 2) Wait until the daemon is back up. Status polls cheaply.
	deadline := time.Now().Add(pollTimeout)
	for {
		if p.DaemonReady() {
			if _, err := p.Manager.Status(ctx); err == nil {
				break
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("config reload: daemon did not come back up within %s", pollTimeout)
		}
		time.Sleep(pollInterval)
	}

	// 3) Re-promote each registered repo so new check rules apply retroactively.
	type repoView struct {
		RepoID  string `json:"repo_id"`
		ShortID string `json:"short_id"`
	}
	var lr struct {
		Repos []repoView `json:"repos"`
	}
	if err := call(ctx, "eng_list_repos", map[string]any{}, &lr); err != nil {
		return fmt.Errorf("config reload: list repos: %w", err)
	}
	if len(lr.Repos) == 0 {
		fmt.Fprintln(p.Out, "no repos registered — nothing to re-scan")
		return nil
	}
	ok, failed := 0, 0
	for _, r := range lr.Repos {
		var resp map[string]any
		if err := call(ctx, "eng_promote_repo", map[string]any{"repo_id": r.RepoID}, &resp); err != nil {
			fmt.Fprintf(p.Out, "  ✗ %s: %v\n", r.ShortID, err)
			failed++
			continue
		}
		fmt.Fprintf(p.Out, "  ✓ %s re-promoted\n", r.ShortID)
		ok++
	}
	fmt.Fprintf(p.Out, "config reload: %d repo(s) ok, %d failed\n", ok, failed)
	if failed > 0 {
		return fmt.Errorf("config reload: %d of %d repos failed", failed, ok+failed)
	}
	return nil
}
