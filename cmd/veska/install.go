package main

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"
	"github.com/whiskeyjimbo/veska/internal/config"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/embedding/model2vec"
)

// installDownloadTimeout bounds the model fetch. potion-code-16M is ~64MB;
// a few minutes covers a slow link without hanging an interactive command.
const installDownloadTimeout = 5 * time.Minute

// potionCode16M is the model directory name (also the model2vec ModelID
// suffix) and the dir under <VeskaHome>/static-model/.
const potionCode16M = "potion-code-16M"

// potionCode16MSpec pins the HuggingFace source for the static code
// embedder. The BaseURL is pinned to a commit revision (not `main`) so
// the download is reproducible; the per-file sha256s are verified after
// fetch, so a moved/edited upstream file fails loudly rather than
// silently embedding against different weights.
func potionCode16MSpec() model2vec.ModelSpec {
	const rev = "86848193a842865570d9c8d3e7d268b66ab52752"
	return model2vec.ModelSpec{
		BaseURL: "https://huggingface.co/minishlab/" + potionCode16M + "/resolve/" + rev,
		Files: []model2vec.FileSpec{
			{Name: "tokenizer.json", SHA256: "8e84217af15e70e8127c855435fc3d8a4cd91d7bbe686f72e75f188118ec78ae"},
			{Name: "model.safetensors", SHA256: "ca6159081a6e96cebe4ad878e5e8437bfccc761e8db16223370149cd2faa6c0b"},
		},
	}
}

// installCmd returns the top-level "install" command grouping model
// fetchers. Today only "model2vec" exists.
func installCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "install",
		Short:        "Install optional models for veska",
		SilenceUsage: true,
	}
	cmd.AddCommand(installModel2vecCmd())
	return cmd
}

// installModel2vecCmd fetches + sha-verifies the potion-code-16M static
// embedder into <VeskaHome>/static-model/, so boot election (solov2-1az)
// picks it up. Idempotent: already-present, sha-matching files are kept.
func installModel2vecCmd() *cobra.Command {
	return &cobra.Command{
		Use:          "model2vec",
		Short:        "Download the model2vec static code embedder (" + potionCode16M + ")",
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			w := cmd.OutOrStdout()
			veskaHome := config.DefaultVectorDir()

			ctx, cancel := context.WithTimeout(cmd.Context(), installDownloadTimeout)
			defer cancel()

			fmt.Fprintf(w, "Installing %s into %s ...\n", potionCode16M,
				model2vec.ModelDir(veskaHome, potionCode16M))
			dir, err := model2vec.Install(ctx, veskaHome, potionCode16M, potionCode16MSpec())
			if err != nil {
				return fmt.Errorf("install model2vec: %w", err)
			}
			fmt.Fprintf(w, "Installed %s to %s\n", potionCode16M, dir)
			fmt.Fprintln(w, "Restart the daemon to elect it as the embedder.")
			return nil
		},
	}
}
