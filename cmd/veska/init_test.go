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
	if err := runInit(context.Background(), deps, true, &buf); err != nil {
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
	err := runInit(context.Background(), deps, true, &buf)
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
	if err := runInit(context.Background(), deps, true, &buf); err != nil {
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
	if err := runInit(context.Background(), deps, true, &buf); err != nil {
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
	if err := runInit(context.Background(), deps, true, &buf); err != nil {
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
