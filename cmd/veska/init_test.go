package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/embedderprobe"
)

// TestResolveVulnChoice_NonInteractiveStdinSkipsPromptAndEchoes pins
// solov2-mgyy: when stdin is non-TTY the prompt must NOT be printed (it
// can't be answered) and the chosen default must be echoed so the
// caller can read the summary and tell vuln scanning was enabled.
func TestResolveVulnChoice_NonInteractiveStdinSkipsPromptAndEchoes(t *testing.T) {
	var out bytes.Buffer
	enabled, err := resolveVulnChoice(initFlags{
		stdin:       strings.NewReader(""),
		interactive: false,
	}, &out)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !enabled {
		t.Errorf("expected default-on for non-interactive, got disabled")
	}
	got := out.String()
	if strings.Contains(got, "[Y/n]") {
		t.Errorf("non-interactive path must NOT print the prompt; got: %q", got)
	}
	if !strings.Contains(got, "OSV") || !strings.Contains(got, "enabled") {
		t.Errorf("non-interactive path must echo the chosen default; got: %q", got)
	}
}

// TestResolveVulnChoice_InteractiveStillPrompts guards that the prompt
// path is unchanged for real TTY callers — `y\n` still accepts and
// `n\n` still declines.
func TestResolveVulnChoice_InteractiveStillPrompts(t *testing.T) {
	var out bytes.Buffer
	enabled, err := resolveVulnChoice(initFlags{
		stdin:       strings.NewReader("n\n"),
		interactive: true,
	}, &out)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if enabled {
		t.Errorf("expected disabled for 'n' answer, got enabled")
	}
	if !strings.Contains(out.String(), "[Y/n]") {
		t.Errorf("interactive path must print the prompt; got: %q", out.String())
	}
}

func TestInitCmdName(t *testing.T) {
	cmd := initCmd()
	if cmd.Name() != "init" {
		t.Fatalf("expected command name %q, got %q", "init", cmd.Name())
	}
}

func TestInitCreatesLayout(t *testing.T) {
	tmp := t.TempDir()

	fakeProbe := func(_ context.Context, _, _ string) (*embedderprobe.ProbeResult, error) {
		return &embedderprobe.ProbeResult{
			Reachable:    true,
			ModelPresent: true,
			EmbedOK:      true,
			Status:       "healthy",
		}, nil
	}

	deps := initDeps{
		veskaHome: tmp,
		probe:     fakeProbe,
		goos:      "linux",
	}

	var buf bytes.Buffer
	if err := runInit(context.Background(), deps, initFlags{yes: true}, &buf); err != nil {
		t.Fatalf("runInit returned error: %v", err)
	}

	for _, sub := range []string{"logs", "cache", "state"} {
		info, err := os.Stat(filepath.Join(tmp, sub))
		if err != nil {
			t.Fatalf("subdir %q not created: %v", sub, err)
		}
		if !info.IsDir() {
			t.Fatalf("%q is not a directory", sub)
		}
	}
}

// TestInitOllamaOverrideDownExitsNonZero: when the user EXPLICITLY forces
// VESKA_EMBEDDER=ollama, init still probes and hard-fails (with the install
// hint) if Ollama is down — the only path that requires Ollama.
func TestInitOllamaOverrideDownExitsNonZero(t *testing.T) {
	tmp := t.TempDir()

	hint := embedderprobe.InstallHint("linux", "nomic-embed-text")

	fakeProbe := func(_ context.Context, _, _ string) (*embedderprobe.ProbeResult, error) {
		return &embedderprobe.ProbeResult{
			Reachable:    false,
			ModelPresent: false,
			EmbedOK:      false,
			InstallHint:  hint,
			Status:       "broken",
		}, nil
	}

	deps := initDeps{
		veskaHome: tmp,
		override:  "ollama",
		probe:     fakeProbe,
		goos:      "linux",
	}

	var buf bytes.Buffer
	err := runInit(context.Background(), deps, initFlags{yes: true}, &buf)
	if err == nil {
		t.Fatal("expected non-nil error when forced Ollama is down, got nil")
	}

	if !strings.Contains(err.Error(), hint) {
		t.Fatalf("expected error to contain install hint %q, got: %v", hint, err)
	}
}

// TestInitAutoSucceedsWithoutOllama: the default (auto) path never touches
// Ollama. With no model installed it elects static-v2, succeeds, and prints
// the model2vec install tip — the probe must NOT be called.
func TestInitAutoSucceedsWithoutOllama(t *testing.T) {
	tmp := t.TempDir()

	probeCalled := false
	deps := initDeps{
		veskaHome: tmp,
		override:  "", // auto
		probe: func(_ context.Context, _, _ string) (*embedderprobe.ProbeResult, error) {
			probeCalled = true
			return &embedderprobe.ProbeResult{Status: "broken"}, nil
		},
		goos: "linux",
	}

	var buf bytes.Buffer
	if err := runInit(context.Background(), deps, initFlags{yes: true}, &buf); err != nil {
		t.Fatalf("auto init should not fail without Ollama: %v", err)
	}
	if probeCalled {
		t.Error("auto path must not probe Ollama")
	}
	out := buf.String()
	if !strings.Contains(out, "static-v2") {
		t.Errorf("expected static-v2 embedder in output, got:\n%s", out)
	}
	if !strings.Contains(out, "veska install model2vec") {
		t.Errorf("expected model2vec install tip, got:\n%s", out)
	}
}

func TestInitSummaryContainsKeyLines(t *testing.T) {
	tmp := t.TempDir()

	fakeProbe := func(_ context.Context, _, _ string) (*embedderprobe.ProbeResult, error) {
		return &embedderprobe.ProbeResult{
			Reachable:    true,
			ModelPresent: true,
			EmbedOK:      true,
			Status:       "healthy",
		}, nil
	}

	deps := initDeps{
		veskaHome: tmp,
		probe:     fakeProbe,
		goos:      "linux",
	}

	var buf bytes.Buffer
	if err := runInit(context.Background(), deps, initFlags{yes: true}, &buf); err != nil {
		t.Fatalf("runInit returned error: %v", err)
	}

	out := buf.String()
	for _, want := range []string{"veska initialized", "ready"} {
		if !strings.Contains(out, want) {
			t.Fatalf("expected output to contain %q, got:\n%s", want, out)
		}
	}
}

// TestInitHintsResolveToRealCommands pins solov2-0ib: every "run: veska …"
// hint the post-init summary prints must name an actual cobra sub-command.
// Drift previously suggested 'veska workspace add .', which never existed.
func TestInitHintsResolveToRealCommands(t *testing.T) {
	tmp := t.TempDir()
	fakeProbe := func(_ context.Context, _, _ string) (*embedderprobe.ProbeResult, error) {
		return &embedderprobe.ProbeResult{
			Reachable: true, ModelPresent: true, EmbedOK: true, Status: "healthy",
		}, nil
	}
	deps := initDeps{veskaHome: tmp, probe: fakeProbe, goos: "linux"}

	var buf bytes.Buffer
	if err := runInit(context.Background(), deps, initFlags{yes: true}, &buf); err != nil {
		t.Fatalf("runInit: %v", err)
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

// TestInitVulnPromptYesEnablesBlock pins solov2-pvyo: --yes accepts the
// default (enabled), so the written config.toml has [vuln_source] live.
func TestInitVulnPromptYesEnablesBlock(t *testing.T) {
	tmp := t.TempDir()
	fakeProbe := func(_ context.Context, _, _ string) (*embedderprobe.ProbeResult, error) {
		return &embedderprobe.ProbeResult{Reachable: true, ModelPresent: true, EmbedOK: true, Status: "healthy"}, nil
	}
	deps := initDeps{veskaHome: tmp, probe: fakeProbe, goos: "linux"}

	var buf bytes.Buffer
	if err := runInit(context.Background(), deps, initFlags{yes: true}, &buf); err != nil {
		t.Fatalf("runInit: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(tmp, "config.toml"))
	if err != nil {
		t.Fatalf("read config.toml: %v", err)
	}
	if !strings.Contains(string(body), "\n[vuln_source]\n") {
		t.Errorf("expected live [vuln_source] block when --yes; got:\n%s", body)
	}
}

// TestInitVulnPromptNoVulnFlagSkipsBlock pins solov2-pvyo: --no-vuln forces
// the disabled (commented-out) shape regardless of --yes.
func TestInitVulnPromptNoVulnFlagSkipsBlock(t *testing.T) {
	tmp := t.TempDir()
	fakeProbe := func(_ context.Context, _, _ string) (*embedderprobe.ProbeResult, error) {
		return &embedderprobe.ProbeResult{Reachable: true, ModelPresent: true, EmbedOK: true, Status: "healthy"}, nil
	}
	deps := initDeps{veskaHome: tmp, probe: fakeProbe, goos: "linux"}

	var buf bytes.Buffer
	if err := runInit(context.Background(), deps, initFlags{yes: true, noVuln: true}, &buf); err != nil {
		t.Fatalf("runInit: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(tmp, "config.toml"))
	if err != nil {
		t.Fatalf("read config.toml: %v", err)
	}
	if strings.Contains(string(body), "\n[vuln_source]\n") {
		t.Errorf("expected commented [vuln_source] when --no-vuln; got:\n%s", body)
	}
	if !strings.Contains(string(body), "# [vuln_source]") {
		t.Errorf("expected commented [vuln_source] hint when --no-vuln; got:\n%s", body)
	}
}

// TestInitVulnPromptInteractiveNo pins solov2-pvyo: interactive 'n' answer
// leaves vuln_source disabled.
func TestInitVulnPromptInteractiveNo(t *testing.T) {
	tmp := t.TempDir()
	fakeProbe := func(_ context.Context, _, _ string) (*embedderprobe.ProbeResult, error) {
		return &embedderprobe.ProbeResult{Reachable: true, ModelPresent: true, EmbedOK: true, Status: "healthy"}, nil
	}
	deps := initDeps{veskaHome: tmp, probe: fakeProbe, goos: "linux"}

	var buf bytes.Buffer
	flags := initFlags{stdin: strings.NewReader("n\n"), interactive: true}
	if err := runInit(context.Background(), deps, flags, &buf); err != nil {
		t.Fatalf("runInit: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(tmp, "config.toml"))
	if err != nil {
		t.Fatalf("read config.toml: %v", err)
	}
	if strings.Contains(string(body), "\n[vuln_source]\n") {
		t.Errorf("expected commented [vuln_source] after 'n' answer; got:\n%s", body)
	}
}
