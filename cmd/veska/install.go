package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
	"github.com/whiskeyjimbo/veska/internal/composition"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/embedding/model2vec"
	"github.com/whiskeyjimbo/veska/internal/platform/config"
)

// installDownloadTimeout bounds the model fetch. potion-code-16M is ~64MB;
// a few minutes covers a slow link without hanging an interactive command.
const installDownloadTimeout = 5 * time.Minute

// installCmd returns the top-level "install" command grouping model
// fetchers. Today only "model2vec" exists.
func installCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "install",
		Short:        "Install optional models for veska",
		SilenceUsage: true,
		// No subcommand is a usage error, not a no-op. Cobra would print help
		// and exit 0 by default; instead return an error so scripts that
		// expect a successful install see a non-zero exit code.
		RunE: func(cmd *cobra.Command, args []string) error {
			_ = cmd.Help()
			return fmt.Errorf("install: missing subcommand (try `veska install model2vec`)")
		},
	}
	cmd.AddCommand(installModel2vecCmd())
	return cmd
}

// installModel2vecCmd fetches + sha-verifies the potion-code-16M static
// embedder into <VeskaHome>/static-model/, so boot election
// picks it up. Idempotent: already-present, sha-matching files are kept.
func installModel2vecCmd() *cobra.Command {
	return &cobra.Command{
		Use:          "model2vec",
		Short:        "Download the model2vec static code embedder (" + composition.PotionCode16MName + ")",
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			w := cmd.OutOrStdout()
			veskaHome := config.DefaultVectorDir()
			modelName := composition.PotionCode16MName

			ctx, cancel := context.WithTimeout(cmd.Context(), installDownloadTimeout)
			defer cancel()

			fmt.Fprintf(w, "Installing %s into %s ...\n", modelName,
				model2vec.ModelDir(veskaHome, modelName))
			dir, err := model2vec.Install(ctx, veskaHome, modelName, composition.PotionCode16MSpec())
			if err != nil {
				return fmt.Errorf("install model2vec: %w", err)
			}
			fmt.Fprintf(w, "Installed %s to %s\n", modelName, dir)
			// only print the restart hint when a daemon is
			// likely running — on a fresh build there is no socket yet
			// and the message confuses first-time users.
			if _, statErr := os.Stat(filepath.Join(veskaHome, "mcp.sock")); statErr == nil {
				fmt.Fprintln(w, "Restart the daemon to elect it as the embedder.")
			}
			return nil
		},
	}
}
