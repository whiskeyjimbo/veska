// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/whiskeyjimbo/veska/internal/cli/diffgatecmd"
)

// addFormatFlag registers the shared --format flag (json|sarif) on a gate
// subcommand. sarif emits SARIF 2.1.0 for GitHub code-scanning; json (default)
// is the historical verdict envelope. Only the blocking gate subcommands get
// this - report and select-tests have their own non-gate output shapes.
func addFormatFlag(cmd *cobra.Command, dst *string) {
	cmd.Flags().StringVar(dst, "format", "json", "output format: json (default) | sarif (GitHub code-scanning)")
}

// validateGateFormat rejects an unrecognized --format value up front, so a typo
// fails loudly instead of silently falling back to JSON.
func validateGateFormat(f string) error {
	if f != "json" && f != "sarif" {
		return fmt.Errorf("--format must be \"json\" or \"sarif\", got %q", f)
	}
	return nil
}

// diffGateCmd is the CI diff-safety gate: index a candidate
// change against the indexed-HEAD graph, verify it resolves its target finding
// within blast radius and introduces no new findings, and emit a machine
// readable pass/fail verdict - exiting non-zero on FAIL for CI gating.
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
	cmd.Flags().StringVar(&branchFlag, "branch", "", "branch (default: repo active branch)")
	cmd.Flags().StringVar(&rootFlag, "repo-root", "", "repo working dir for git ref reads (default: cwd)")
	cmd.Flags().StringVar(&baseRef, "base-ref", "", "git ref of the base the candidate is diffed against")
	cmd.Flags().StringVar(&candRef, "candidate-ref", "", "git ref/worktree of the candidate change")
	cmd.Flags().StringVar(&finding, "finding", "", "target finding id from 'veska findings list'; derives --anchor and --rule")
	cmd.Flags().StringVar(&anchor, "anchor", "", "node id the target finding is anchored on (alternative to --finding)")
	cmd.Flags().StringVar(&rule, "rule", "", "rule name of the target finding, e.g. dead-code (alternative to --finding)")
	cmd.AddCommand(diffGateClonesCmd())
	cmd.AddCommand(diffGateSecurityCmd())
	cmd.AddCommand(diffGateUntestedCmd())
	cmd.AddCommand(diffGateCyclesCmd())
	cmd.AddCommand(diffGateAPICmd())
	cmd.AddCommand(diffGateReportCmd())
	cmd.AddCommand(diffGateSelectTestsCmd())
	return cmd
}

// diffGateReportCmd is the advisory PR impact/risk report: NOT a
// gate. It assembles, for a diff, the blast radius, each changed file's
// change-risk standing, open findings on the touched files, and the
// changed-but-untested symbols - and ALWAYS exits 0 (presence of findings/risk
// never blocks; an un-indexed repo yields a noted report). The soft on-ramp:
// teams trust an advisory "what this diff touches / where it's risky" before
// they let the graph block a merge.
func diffGateReportCmd() *cobra.Command {
	var (
		repoFlag   string
		branchFlag string
		rootFlag   string
		baseRef    string
		candRef    string
	)
	cmd := &cobra.Command{
		Use:          "report",
		Short:        "Advisory PR impact/risk report (blast radius, change-risk, findings, untested) - always exits 0, never gates",
		Long:         "Assemble an ADVISORY report for a candidate change (base-ref..candidate-ref): the diff's blast radius, each changed file's change-risk standing (recent-change-frequency × blast-radius), open findings on the touched files, and changed-but-untested symbols. Unlike the diff-gate subcommands this NEVER gates - it always exits 0 (findings/risk never block; an un-indexed repo or a failed section yields a noted report). The soft on-ramp before teams trust blocking gates. Emits JSON.",
		Example:      "  veska diff-gate report --repo <id> --base-ref HEAD~1 --candidate-ref HEAD",
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
			return diffgatecmd.RunReport(cmd.Context(), diffgatecmd.ReportParams{
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
	cmd.Flags().StringVar(&branchFlag, "branch", "", "branch (default: repo active branch)")
	cmd.Flags().StringVar(&rootFlag, "repo-root", "", "repo working dir for git ref reads (default: cwd)")
	cmd.Flags().StringVar(&baseRef, "base-ref", "", "git ref of the base the candidate is diffed against")
	cmd.Flags().StringVar(&candRef, "candidate-ref", "", "git ref/worktree of the candidate change")
	return cmd
}

// diffGateAPICmd is the breaking-exported-signature diff-twin gate
// a blanket gate (no target finding) that FAILs when the
// candidate changes the signature SHAPE of an exported symbol. It reuses the
// contract-drift signal over the re-promoted candidate and filters to the
// exported visibility flag, so unexported and body-only changes pass. Scope is
// signature-shape only: symbol REMOVAL/RENAME is not detected (delete-replace
// emits no drift), and "exported" is the name-based
// flag, not a reachability analysis.
func diffGateAPICmd() *cobra.Command {
	var (
		repoFlag   string
		branchFlag string
		rootFlag   string
		baseRef    string
		candRef    string
		fmtFlag    string
	)
	cmd := &cobra.Command{
		Use:          "api",
		Short:        "Gate a candidate change on breaking exported-signature changes, emit pass/fail JSON (exits non-zero on FAIL)",
		Long:         "Index a candidate change (base-ref..candidate-ref) against the indexed-HEAD graph and FAIL when it changes the signature shape (name + parameters + result) of an EXPORTED symbol - a breaking public-surface change. Unexported signature changes and body-only edits pass. Scope is signature-shape only: symbol removal/rename is not detected, and exported is the name-based visibility flag, not reachability. Deterministic, network-free; emits JSON and exits non-zero on FAIL for CI gating.",
		Example:      "  veska diff-gate api --repo <id> --base-ref HEAD~1 --candidate-ref HEAD",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := validateGateFormat(fmtFlag); err != nil {
				return err
			}
			root := rootFlag
			if root == "" {
				wd, err := os.Getwd()
				if err != nil {
					return err
				}
				root = wd
			}
			return diffgatecmd.RunAPIBreak(cmd.Context(), diffgatecmd.APIParams{
				RepoID:       repoFlag,
				Branch:       branchFlag,
				RepoRoot:     root,
				BaseRef:      baseRef,
				CandidateRef: candRef,
				Format:       fmtFlag,
				Out:          cmd.OutOrStdout(),
			})
		},
	}
	cmd.Flags().StringVar(&repoFlag, "repo", "", "repo id (scopes the indexed-HEAD base graph)")
	cmd.Flags().StringVar(&branchFlag, "branch", "", "branch (default: repo active branch)")
	cmd.Flags().StringVar(&rootFlag, "repo-root", "", "repo working dir for git ref reads (default: cwd)")
	cmd.Flags().StringVar(&baseRef, "base-ref", "", "git ref of the base the candidate is diffed against")
	cmd.Flags().StringVar(&candRef, "candidate-ref", "", "git ref/worktree of the candidate change")
	addFormatFlag(cmd, &fmtFlag)
	return cmd
}

// diffGateCyclesCmd is the dependency-cycle diff-twin gate: a
// blanket gate (no target finding) that FAILs when the candidate introduces a
// net-new dependency cycle - a strongly-connected component of >=2 symbols over
// CALLS/IMPORTS edges that was not already a single cycle at base. Node-level and
// language-agnostic; on compiling Go the catchable case is within-package mutual
// recursion (the compiler forbids package import cycles), but the same gate
// catches the import cycles other languages permit once their parsers emit
// IMPORTS edges.
func diffGateCyclesCmd() *cobra.Command {
	var (
		repoFlag   string
		branchFlag string
		rootFlag   string
		baseRef    string
		candRef    string
		fmtFlag    string
	)
	cmd := &cobra.Command{
		Use:          "cycles",
		Short:        "Gate a candidate change on newly-introduced dependency cycles, emit pass/fail JSON (exits non-zero on FAIL)",
		Long:         "Index a candidate change (base-ref..candidate-ref) against the indexed-HEAD graph and FAIL when it introduces a net-new dependency cycle - a strongly-connected component of >=2 symbols (over CALLS/IMPORTS edges) absent at base. The candidate is re-promoted so cross-file edges resolve; only cycles touching the change set are judged. Node-level, deterministic, network-free; emits JSON and exits non-zero on FAIL for CI gating.",
		Example:      "  veska diff-gate cycles --repo <id> --base-ref HEAD~1 --candidate-ref HEAD",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := validateGateFormat(fmtFlag); err != nil {
				return err
			}
			root := rootFlag
			if root == "" {
				wd, err := os.Getwd()
				if err != nil {
					return err
				}
				root = wd
			}
			return diffgatecmd.RunCycles(cmd.Context(), diffgatecmd.CycleParams{
				RepoID:       repoFlag,
				Branch:       branchFlag,
				RepoRoot:     root,
				BaseRef:      baseRef,
				CandidateRef: candRef,
				Format:       fmtFlag,
				Out:          cmd.OutOrStdout(),
			})
		},
	}
	cmd.Flags().StringVar(&repoFlag, "repo", "", "repo id (scopes the indexed-HEAD base graph)")
	cmd.Flags().StringVar(&branchFlag, "branch", "", "branch (default: repo active branch)")
	cmd.Flags().StringVar(&rootFlag, "repo-root", "", "repo working dir for git ref reads (default: cwd)")
	cmd.Flags().StringVar(&baseRef, "base-ref", "", "git ref of the base the candidate is diffed against")
	cmd.Flags().StringVar(&candRef, "candidate-ref", "", "git ref/worktree of the candidate change")
	addFormatFlag(cmd, &fmtFlag)
	return cmd
}

// diffGateUntestedCmd is the diff-coverage gate: a blanket gate
// that FAILs when the candidate changes or adds a prod symbol no test reaches.
// Coverage is a CALLS-edge proxy (a test-file caller), not real coverage data;
// it re-promotes the candidate so a test added in the same diff counts.
func diffGateUntestedCmd() *cobra.Command {
	var (
		repoFlag   string
		branchFlag string
		rootFlag   string
		baseRef    string
		candRef    string
		fmtFlag    string
	)
	cmd := &cobra.Command{
		Use:          "untested",
		Short:        "Gate a candidate change on changed prod symbols that no test reaches, emit pass/fail JSON (exits non-zero on FAIL)",
		Long:         "Index a candidate change (base-ref..candidate-ref) and FAIL when a changed or added prod symbol has no test-file caller in the candidate after-state - a CALLS-edge coverage proxy, not real coverage data. The candidate is re-promoted so a test added in the same diff counts; only symbols in the change set are judged. Emits JSON and exits non-zero on FAIL for CI gating.",
		Example:      "  veska diff-gate untested --repo <id> --base-ref HEAD~1 --candidate-ref HEAD",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := validateGateFormat(fmtFlag); err != nil {
				return err
			}
			root := rootFlag
			if root == "" {
				wd, err := os.Getwd()
				if err != nil {
					return err
				}
				root = wd
			}
			return diffgatecmd.RunUntested(cmd.Context(), diffgatecmd.UntestedParams{
				RepoID:       repoFlag,
				Branch:       branchFlag,
				RepoRoot:     root,
				BaseRef:      baseRef,
				CandidateRef: candRef,
				Format:       fmtFlag,
				Out:          cmd.OutOrStdout(),
			})
		},
	}
	cmd.Flags().StringVar(&repoFlag, "repo", "", "repo id (scopes the indexed-HEAD base graph)")
	cmd.Flags().StringVar(&branchFlag, "branch", "", "branch (default: repo active branch)")
	cmd.Flags().StringVar(&rootFlag, "repo-root", "", "repo working dir for git ref reads (default: cwd)")
	cmd.Flags().StringVar(&baseRef, "base-ref", "", "git ref of the base the candidate is diffed against")
	cmd.Flags().StringVar(&candRef, "candidate-ref", "", "git ref/worktree of the candidate change")
	addFormatFlag(cmd, &fmtFlag)
	return cmd
}

// diffGateSelectTestsCmd is impact-based test selection: NOT a
// gate - it emits the tests whose covered nodes intersect the diff's changed
// prod nodes as a `go test -run` selection, always exiting 0.
func diffGateSelectTestsCmd() *cobra.Command {
	var (
		repoFlag   string
		branchFlag string
		rootFlag   string
		baseRef    string
		candRef    string
	)
	cmd := &cobra.Command{
		Use:          "select-tests",
		Short:        "Select the tests whose covered nodes intersect a diff (emit go test -run per package); never gates",
		Long:         "Select the tests whose covered nodes intersect a candidate change (base-ref..candidate-ref) and emit a runner-consumable `go test -run` selection per package. Covering tests are derived transitively from the latent *_test.go CALLS edges already in the index (no real coverage data) - a selection HEURISTIC that over-selects (the safe direction), not a guarantee. Changed test files force their whole package since their tests may not be indexed yet. NEVER gates: every selection outcome - including unknown-repo, repo-not-indexed, and bad-ref - exits 0 with a JSON envelope (an advisory reason lands in the `error` field); only a usage or infrastructure error exits non-zero. Always emits JSON.",
		Example:      "  veska diff-gate select-tests --repo <id> --base-ref HEAD~1 --candidate-ref HEAD",
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
			return diffgatecmd.RunSelectTests(cmd.Context(), diffgatecmd.SelectTestsParams{
				RepoID:       repoFlag,
				Branch:       branchFlag,
				RepoRoot:     root,
				BaseRef:      baseRef,
				CandidateRef: candRef,
				Out:          cmd.OutOrStdout(),
			})
		},
	}
	cmd.Flags().StringVar(&repoFlag, "repo", "", "repo id (scopes the indexed base graph)")
	cmd.Flags().StringVar(&branchFlag, "branch", "", "branch (default: repo active branch)")
	cmd.Flags().StringVar(&rootFlag, "repo-root", "", "repo working dir for git ref reads (default: cwd)")
	cmd.Flags().StringVar(&baseRef, "base-ref", "", "git ref of the base the candidate is diffed against")
	cmd.Flags().StringVar(&candRef, "candidate-ref", "", "git ref/worktree of the candidate change")
	return cmd
}

// diffGateSecurityCmd is the net-new security diff-twin gate: a
// blanket gate (no target finding, no indexed graph) that FAILs when the
// candidate introduces a new secret_leak (added-line scan, language-agnostic)
// or vulnerable_dependency (manifest finding-delta; Go/go.mod today) finding.
func diffGateSecurityCmd() *cobra.Command {
	var (
		repoFlag   string
		branchFlag string
		rootFlag   string
		baseRef    string
		candRef    string
		fmtFlag    string
	)
	cmd := &cobra.Command{
		Use:          "security",
		Short:        "Gate a candidate change on net-new secret/vulnerable-dependency findings, emit pass/fail JSON (exits non-zero on FAIL)",
		Long:         "Scan a candidate change (base-ref..candidate-ref) and FAIL when it introduces a new secret_leak (scanned over the diff's added lines - any language) or a new vulnerable_dependency (manifest finding-delta by finding_id; go.mod today). A blanket gate: no target finding, no indexed graph required. Offline and deterministic; emits JSON and exits non-zero on FAIL for CI gating.",
		Example:      "  veska diff-gate security --repo <id> --base-ref HEAD~1 --candidate-ref HEAD",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := validateGateFormat(fmtFlag); err != nil {
				return err
			}
			root := rootFlag
			if root == "" {
				wd, err := os.Getwd()
				if err != nil {
					return err
				}
				root = wd
			}
			return diffgatecmd.RunSecurity(cmd.Context(), diffgatecmd.SecurityParams{
				RepoID:       repoFlag,
				Branch:       branchFlag,
				RepoRoot:     root,
				BaseRef:      baseRef,
				CandidateRef: candRef,
				Format:       fmtFlag,
				Out:          cmd.OutOrStdout(),
			})
		},
	}
	cmd.Flags().StringVar(&repoFlag, "repo", "", "repo id (folded into finding keys)")
	cmd.Flags().StringVar(&branchFlag, "branch", "", "branch (default: repo active branch)")
	cmd.Flags().StringVar(&rootFlag, "repo-root", "", "repo working dir for git ref reads (default: cwd)")
	cmd.Flags().StringVar(&baseRef, "base-ref", "", "git ref of the base the candidate is diffed against")
	cmd.Flags().StringVar(&candRef, "candidate-ref", "", "git ref/worktree of the candidate change")
	addFormatFlag(cmd, &fmtFlag)
	return cmd
}

// diffGateClonesCmd is the exact-clone diff-twin gate: a
// blanket gate (no target finding) that FAILs when the candidate introduces a
// byte-identical copy of existing code - net-new exact-clone duplication absent
// at base. Deterministic and embedding-free (content_hash equality); near-mode
// is out by design.
func diffGateClonesCmd() *cobra.Command {
	var (
		repoFlag   string
		branchFlag string
		rootFlag   string
		baseRef    string
		candRef    string
		fmtFlag    string
	)
	cmd := &cobra.Command{
		Use:          "clones",
		Short:        "Gate a candidate change on newly-introduced exact-clone duplication, emit pass/fail JSON (exits non-zero on FAIL)",
		Long:         "Index a candidate change (base-ref..candidate-ref) against the indexed-HEAD graph and FAIL when it introduces a new exact-clone group - a byte-identical copy (content_hash equality) of code it did not already duplicate at base. Deterministic, network-free, embedding-free; emits JSON and exits non-zero on FAIL for CI gating.",
		Example:      "  veska diff-gate clones --repo <id> --base-ref HEAD~1 --candidate-ref HEAD",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := validateGateFormat(fmtFlag); err != nil {
				return err
			}
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
				Format:       fmtFlag,
				Out:          cmd.OutOrStdout(),
			})
		},
	}
	cmd.Flags().StringVar(&repoFlag, "repo", "", "repo id (scopes the indexed-HEAD base graph)")
	cmd.Flags().StringVar(&branchFlag, "branch", "", "branch (default: repo active branch)")
	cmd.Flags().StringVar(&rootFlag, "repo-root", "", "repo working dir for git ref reads (default: cwd)")
	cmd.Flags().StringVar(&baseRef, "base-ref", "", "git ref of the base the candidate is diffed against")
	cmd.Flags().StringVar(&candRef, "candidate-ref", "", "git ref/worktree of the candidate change")
	addFormatFlag(cmd, &fmtFlag)
	return cmd
}
