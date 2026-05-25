package main

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"
	"github.com/whiskeyjimbo/veska/internal/service"
)

// configCmd is the parent for `veska config …`.
//
// solov2-oqlr: opt-in features that need the [vuln_source] block in
// ~/.veska/config.toml require a daemon restart AND a re-scan of every
// already-promoted repo to surface new findings retroactively. Without
// this command a user has to chain three separate calls
// (service stop → service start → reindex <path> for every repo). The
// reload subcommand turns that into one.
func configCmd(mgr service.Manager) *cobra.Command {
	cmd := &cobra.Command{
		Use:          "config",
		Short:        "Manage veska configuration",
		SilenceUsage: true,
	}
	cmd.AddCommand(configReloadCmd(mgr))
	return cmd
}

func configReloadCmd(mgr service.Manager) *cobra.Command {
	return &cobra.Command{
		Use:          "reload",
		Short:        "Restart the daemon and re-promote every registered repo so new config takes effect",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			w := cmd.OutOrStdout()
			ctx := cmd.Context()
			if mgr == nil {
				return errNoManager
			}

			// 1) Restart so the daemon re-reads ~/.veska/config.toml.
			fmt.Fprintln(w, "restarting daemon to pick up config changes...")
			if err := mgr.Restart(ctx); err != nil {
				return fmt.Errorf("config reload: restart: %w", err)
			}

			// 2) Wait until the daemon is back up. Status polls cheaply.
			deadline := time.Now().Add(15 * time.Second)
			for {
				if daemonRunning() {
					if _, err := mgr.Status(ctx); err == nil {
						break
					}
				}
				if time.Now().After(deadline) {
					return fmt.Errorf("config reload: daemon did not come back up within 15s")
				}
				time.Sleep(250 * time.Millisecond)
			}

			// 3) Re-promote each registered repo so check rules added by the
			//    new config (notably vuln-scan when [vuln_source] is on)
			//    surface findings on already-promoted code.
			type repoView struct {
				RepoID  string `json:"repo_id"`
				ShortID string `json:"short_id"`
			}
			type listResult struct {
				Repos []repoView `json:"repos"`
			}
			var lr listResult
			if err := callMCP(ctx, "eng_list_repos", map[string]any{}, &lr); err != nil {
				return fmt.Errorf("config reload: list repos: %w", err)
			}
			if len(lr.Repos) == 0 {
				fmt.Fprintln(w, "no repos registered — nothing to re-scan")
				return nil
			}
			ok, failed := 0, 0
			for _, r := range lr.Repos {
				var resp map[string]any
				if err := callMCP(ctx, "eng_promote_repo", map[string]any{"repo_id": r.RepoID}, &resp); err != nil {
					fmt.Fprintf(w, "  ✗ %s: %v\n", r.ShortID, err)
					failed++
					continue
				}
				fmt.Fprintf(w, "  ✓ %s re-promoted\n", r.ShortID)
				ok++
			}
			fmt.Fprintf(w, "config reload: %d repo(s) ok, %d failed\n", ok, failed)
			if failed > 0 {
				return fmt.Errorf("config reload: %d of %d repos failed", failed, ok+failed)
			}
			return nil
		},
	}
}

