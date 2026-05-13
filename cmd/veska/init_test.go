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

func TestInitOllamaDownExitsNonZero(t *testing.T) {
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
		probe:     fakeProbe,
		goos:      "linux",
	}

	var buf bytes.Buffer
	err := runInit(context.Background(), deps, true, &buf)
	if err == nil {
		t.Fatal("expected non-nil error when Ollama is down, got nil")
	}

	if !strings.Contains(err.Error(), hint) {
		t.Fatalf("expected error to contain install hint %q, got: %v", hint, err)
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
