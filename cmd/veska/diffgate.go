package main

import (
	"os"

	"github.com/spf13/cobra"

	"github.com/whiskeyjimbo/veska/internal/cli/diffgatecmd"
)

// diffGateCmd is the CI diff-safety gate (solov2-ll57.2): index a candidate
// change against the indexed-HEAD graph, verify it resolves its target finding
// within blast radius and introduces no new findings, and emit a machine-
// readable pass/fail verdict — exiting non-zero on FAIL for CI gating.
//
// Structural finding-discovery (dead-code, contract-drift) is wired; a change
// that introduces a new structural finding FAILs. The target finding is
// supplied as flags (--anchor/--rule) rather than looked up from storage.
func diffGateCmd() *cobra.Command {
	var (
		repoFlag   string
		branchFlag string
		rootFlag   string
		baseRef    string
		candRef    string
		anchor     string
		rule       string
	)
	cmd := &cobra.Command{
		Use:          "diff-gate",
		Short:        "Gate a candidate change: verify it resolves a finding within blast radius, emit pass/fail JSON (exits non-zero on FAIL)",
		Long:         "Index a candidate change (base-ref..candidate-ref) against the indexed-HEAD graph and emit a deterministic, network-free pass/fail verdict: did it resolve its target finding, introduce no new structural findings (dead-code, contract-drift), and stay within the finding's blast radius? Emits JSON and exits non-zero on FAIL for CI gating.",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			root := rootFlag
			if root == "" {
				wd, err := os.Getwd()
				if err != nil {
					return err
				}
				root = wd
			}
			return diffgatecmd.Run(cmd.Context(), diffgatecmd.Params{
				RepoID:       repoFlag,
				Branch:       branchFlag,
				RepoRoot:     root,
				BaseRef:      baseRef,
				CandidateRef: candRef,
				AnchorNodeID: anchor,
				Rule:         rule,
				Out:          cmd.OutOrStdout(),
			})
		},
	}
	cmd.Flags().StringVar(&repoFlag, "repo", "", "repo id (scopes the indexed-HEAD base graph)")
	cmd.Flags().StringVar(&branchFlag, "branch", "main", "branch")
	cmd.Flags().StringVar(&rootFlag, "repo-root", "", "repo working dir for git ref reads (default: cwd)")
	cmd.Flags().StringVar(&baseRef, "base-ref", "", "git ref of the base the candidate is diffed against")
	cmd.Flags().StringVar(&candRef, "candidate-ref", "", "git ref/worktree of the candidate change")
	cmd.Flags().StringVar(&anchor, "anchor", "", "node id the target finding is anchored on")
	cmd.Flags().StringVar(&rule, "rule", "", "rule name of the target finding (e.g. dead-code)")
	return cmd
}
