// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/cli/initcmd"
	"github.com/whiskeyjimbo/veska/internal/platform/embedderprobe"
)

// The init flow's unit tests live in internal/cli/initcmd; these cover the
// Cobra-layer wiring that only exists in package main (the constructed command
// and the root --help / command-list surface).

func TestInitCmdName(t *testing.T) {
	cmd := initCmd()
	if cmd.Name() != "init" {
		t.Fatalf("expected command name %q, got %q", "init", cmd.Name())
	}
}

// TestInitHintsResolveToRealCommands pins: every "run: veska …"
// hint the post-init summary prints must name an actual cobra sub-command.
// Drift previously suggested 'veska workspace add.', which never existed.
func TestInitHintsResolveToRealCommands(t *testing.T) {
	tmp := t.TempDir()
	fakeProbe := func(_ context.Context, _, _ string) (*embedderprobe.ProbeResult, error) {
		return &embedderprobe.ProbeResult{
			Reachable: true, ModelPresent: true, EmbedOK: true, Status: "healthy",
		}, nil
	}
	deps := initcmd.Deps{VeskaHome: tmp, Probe: fakeProbe, GOOS: "linux"}

	var buf bytes.Buffer
	if err := initcmd.Run(context.Background(), deps, initcmd.Flags{Yes: true}, &buf); err != nil {
		t.Fatalf("initcmd.Run: %v", err)
	}

	root := newRootCmd()
	available := map[string]bool{}
	for _, c := range root.Commands() {
		available[c.Name()] = true
	}

	// Each hint must reference a real top-level command. Add hints here as
	// they appear in the summary.
	for _, want := range []string{"veska repo", "veska service"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("summary missing hint %q; output:\n%s", want, buf.String())
		}
		// "veska repo" → command "repo"; "veska service" → command "service".
		cmd := strings.TrimPrefix(want, "veska ")
		if !available[cmd] {
			t.Errorf("hint %q references missing sub-command %q (available: %v)",
				want, cmd, available)
		}
	}
}

// TestRootHelpMentionsInit pins #2: `veska --help` must
// surface a "run veska init to set up" hint so a brand-new user knows
// the entry point without scanning 30 alphabetised sub-commands.
func TestRootHelpMentionsInit(t *testing.T) {
	root := newRootCmd()
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs([]string{"--help"})
	if err := root.Execute(); err != nil {
		t.Fatalf("--help returned error: %v", err)
	}
	out := buf.String()
	// The hint must point users at `veska init` as the start-here step
	// distinct from `init` merely appearing in the alphabetised command list.
	if !strings.Contains(out, `run "veska init"`) {
		t.Errorf("expected --help to include a 'run \"veska init\" to set up' hint; got:\n%s", out)
	}
}
