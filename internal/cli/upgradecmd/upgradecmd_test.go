// SPDX-License-Identifier: AGPL-3.0-only

package upgradecmd

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeExe(t *testing.T, path string, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0755); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestRunSwapsAndRestarts(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "new")
	target := filepath.Join(dir, "old")
	writeExe(t, src, "NEW")
	writeExe(t, target, "OLD")

	restarted := false
	err := Run(context.Background(), Params{
		Source:    src,
		Target:    target,
		Restart:   true,
		Out:       &bytes.Buffer{},
		RestartFn: func(context.Context) error { restarted = true; return nil },
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	got, _ := os.ReadFile(target)
	if string(got) != "NEW" {
		t.Errorf("target content = %q, want NEW", got)
	}
	if !restarted {
		t.Error("RestartFn was not called")
	}
}

func TestRunRestartWithoutManager(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "new")
	target := filepath.Join(dir, "old")
	writeExe(t, src, "NEW")
	writeExe(t, target, "OLD")

	err := Run(context.Background(), Params{
		Source:  src,
		Target:  target,
		Restart: true,
		Out:     &bytes.Buffer{},
		// RestartFn nil -> --restart must report ErrNoManager.
	})
	if !errors.Is(err, ErrNoManager) {
		t.Fatalf("want ErrNoManager, got %v", err)
	}
}

func TestRunRejectsNonExecutableSource(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "new")
	if err := os.WriteFile(src, []byte("data"), 0644); err != nil {
		t.Fatal(err)
	}
	err := Run(context.Background(), Params{Source: src, Target: filepath.Join(dir, "old"), Out: &bytes.Buffer{}})
	if err == nil || !strings.Contains(err.Error(), "executable") {
		t.Fatalf("want non-executable error, got %v", err)
	}
}
