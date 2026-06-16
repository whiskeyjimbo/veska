package main

import (
	"fmt"
	"os"
	"runtime"
	"strings"

	"github.com/spf13/cobra"

	"github.com/whiskeyjimbo/veska/internal/cli/initcmd"
	"github.com/whiskeyjimbo/veska/internal/platform/config"
	"github.com/whiskeyjimbo/veska/internal/platform/embedderprobe"
)

// The init flow (first-run machine setup) and the --agent snippet writer live
// in internal/cli/initcmd; this file is Cobra glue whose RunE builds the
// initcmd.Deps/Flags from the environment and delegates.

// initCmd returns the "init" Cobra command that runs the first-run flow.
func initCmd() *cobra.Command {
	var yes bool
	var noVuln bool
	var agent string
	var updateGitignore bool

	cmd := &cobra.Command{
		Use:          "init",
		Short:        "First-run setup, or write per-agent instruction snippet with --agent",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			// agent is project-scoped and short-circuits the
			// machine-scoped first-run flow: the two intentionally
			// don't co-execute.
			if agent != "" {
				cwd, err := os.Getwd()
				if err != nil {
					return fmt.Errorf("init --agent: cwd: %w", err)
				}
				return initcmd.WriteAgentSnippet(initcmd.AgentSnippetParams{
					RootDir:         cwd,
					Flavor:          agent,
					Out:             cmd.OutOrStdout(),
					In:              cmd.InOrStdin(),
					UpdateGitignore: updateGitignore,
					AssumeYes:       yes,
					Interactive:     initcmd.StdinIsInteractive(),
				})
			}
			deps := initcmd.Deps{
				VeskaHome: config.DefaultVectorDir(),
				Override:  os.Getenv("VESKA_EMBEDDER"),
				Probe:     embedderprobe.Probe,
				GOOS:      runtime.GOOS,
			}
			flags := initcmd.Flags{
				Yes:         yes,
				NoVuln:      noVuln,
				Stdin:       cmd.InOrStdin(),
				Interactive: initcmd.StdinIsInteractive(),
			}
			return initcmd.Run(cmd.Context(), deps, flags, cmd.OutOrStdout())
		},
	}

	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "auto-accept all prompts (non-interactive mode)")
	cmd.Flags().BoolVar(&noVuln, "no-vuln", false, "skip the OSV vulnerability-scanner prompt and leave it disabled")
	cmd.Flags().StringVar(&agent, "agent", "",
		"write a per-agent instruction snippet to the current project ("+
			strings.Join(initcmd.SupportedFlavorNames(), ", ")+")")
	cmd.Flags().BoolVar(&updateGitignore, "update-gitignore", false,
		"with --agent: also write a veska-managed block to .gitignore covering generated artifacts (off by default)")
	return cmd
}
