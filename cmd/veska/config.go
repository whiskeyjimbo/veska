package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/spf13/cobra"
	"github.com/whiskeyjimbo/veska/internal/cli/mcpclient"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/embedding/elect"
	"github.com/whiskeyjimbo/veska/internal/platform/config"
	"github.com/whiskeyjimbo/veska/internal/platform/service"
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
	cmd.AddCommand(configShowCmd())
	return cmd
}

// configShowCmd prints the effective resolved config: defaults merged with
// ~/.veska/config.toml and env-var overrides — same pipeline the daemon
// uses at boot, so the operator sees the EXACT shape the daemon will
// observe (solov2-p6rt). Read-only; the write-side subcommands
// (set/enable/disable) are deferred behind a follow-up bead because
// BurntSushi/toml v1.6 loses comments on marshal.
func configShowCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:          "show",
		Short:        "Print the effective veska configuration (defaults + config.toml + env)",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return fmt.Errorf("config show: %w", err)
			}
			w := cmd.OutOrStdout()
			liveEmbedder := elect.Marker(config.DefaultVectorDir())
			if jsonOut {
				// Sibling key `_live_embedder` carries the daemon's
				// elected embedder so callers don't read the [embedder]
				// defaults (which only apply on VESKA_EMBEDDER=ollama)
				// as the truth. Empty string when no election has run
				// yet (solov2-awp6).
				envelope := struct {
					*config.Config
					LiveEmbedder string `json:"_live_embedder,omitempty"`
				}{&cfg, liveEmbedder}
				enc := json.NewEncoder(w)
				enc.SetIndent("", "  ")
				return enc.Encode(envelope)
			}
			if liveEmbedder != "" {
				fmt.Fprintf(w, "# live embedder: %s\n", liveEmbedder)
				fmt.Fprintf(w, "# the [embedder] block below configures the Ollama branch and is\n")
				fmt.Fprintf(w, "# unused unless VESKA_EMBEDDER=ollama.\n\n")
			}
			var buf bytes.Buffer
			if err := toml.NewEncoder(&buf).Encode(cfg); err != nil {
				return fmt.Errorf("config show: encode toml: %w", err)
			}
			_, werr := w.Write(buf.Bytes())
			return werr
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSON instead of TOML")
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
			if err := mcpclient.Call(ctx, "eng_list_repos", map[string]any{}, &lr); err != nil {
				return fmt.Errorf("config reload: list repos: %w", err)
			}
			if len(lr.Repos) == 0 {
				fmt.Fprintln(w, "no repos registered — nothing to re-scan")
				return nil
			}
			ok, failed := 0, 0
			for _, r := range lr.Repos {
				var resp map[string]any
				if err := mcpclient.Call(ctx, "eng_promote_repo", map[string]any{"repo_id": r.RepoID}, &resp); err != nil {
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
