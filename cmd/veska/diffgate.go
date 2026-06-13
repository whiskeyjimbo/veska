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
		finding    string
		anchor     string
		rule       string
	)
	cmd := &cobra.Command{
		Use:          "diff-gate",
		Short:        "Gate a candidate change: verify it resolves a finding within blast radius, emit pass/fail JSON (exits non-zero on FAIL)",
		Long:         "Index a candidate change (base-ref..candidate-ref) against the indexed-HEAD graph and emit a deterministic, network-free pass/fail verdict: did it resolve its target finding, introduce no new structural findings (dead-code, contract-drift), and stay within the finding's blast radius? Emits JSON and exits non-zero on FAIL for CI gating.\n\nIdentify the target finding with --finding <id> (the first column of `veska findings list`); its anchor and rule are derived for you. Power users / CI can pass --anchor + --rule directly instead.",
		Example:      "  # gate a fix for a finding you saw in `veska findings list`\n  veska diff-gate --repo <id> --finding <finding_id> --base-ref HEAD~1 --candidate-ref HEAD",
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
				FindingID:    finding,
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
	cmd.Flags().StringVar(&finding, "finding", "", "target finding id from 'veska findings list'; derives --anchor and --rule")
	cmd.Flags().StringVar(&anchor, "anchor", "", "node id the target finding is anchored on (alternative to --finding)")
	cmd.Flags().StringVar(&rule, "rule", "", "rule name of the target finding, e.g. dead-code (alternative to --finding)")
	cmd.AddCommand(diffGateClonesCmd())
	return cmd
}

// diffGateClonesCmd is the exact-clone diff-twin gate (solov2-zvh6.7): a
// blanket gate (no target finding) that FAILs when the candidate introduces a
// byte-identical copy of existing code — net-new exact-clone duplication absent
// at base. Deterministic and embedding-free (content_hash equality); near-mode
// is out by design.
func diffGateClonesCmd() *cobra.Command {
	var (
		repoFlag   string
		branchFlag string
		rootFlag   string
		baseRef    string
		candRef    string
	)
	cmd := &cobra.Command{
		Use:          "clones",
		Short:        "Gate a candidate change on newly-introduced exact-clone duplication, emit pass/fail JSON (exits non-zero on FAIL)",
		Long:         "Index a candidate change (base-ref..candidate-ref) against the indexed-HEAD graph and FAIL when it introduces a new exact-clone group — a byte-identical copy (content_hash equality) of code it did not already duplicate at base. Deterministic, network-free, embedding-free; emits JSON and exits non-zero on FAIL for CI gating.",
		Example:      "  veska diff-gate clones --repo <id> --base-ref HEAD~1 --candidate-ref HEAD",
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
			return diffgatecmd.RunClones(cmd.Context(), diffgatecmd.CloneParams{
				RepoID:       repoFlag,
				Branch:       branchFlag,
				RepoRoot:     root,
				BaseRef:      baseRef,
				CandidateRef: candRef,
				Out:          cmd.OutOrStdout(),
			})
		},
	}
	cmd.Flags().StringVar(&repoFlag, "repo", "", "repo id (scopes the indexed-HEAD base graph)")
	cmd.Flags().StringVar(&branchFlag, "branch", "main", "branch")
	cmd.Flags().StringVar(&rootFlag, "repo-root", "", "repo working dir for git ref reads (default: cwd)")
	cmd.Flags().StringVar(&baseRef, "base-ref", "", "git ref of the base the candidate is diffed against")
	cmd.Flags().StringVar(&candRef, "candidate-ref", "", "git ref/worktree of the candidate change")
	return cmd
}
