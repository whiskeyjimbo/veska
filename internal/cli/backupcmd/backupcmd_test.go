// SPDX-License-Identifier: AGPL-3.0-only

package backupcmd

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func params(out *bytes.Buffer, dir string, jsonOut bool) ListParams {
	return ListParams{
		BackupDir:   dir,
		JSONOut:     jsonOut,
		Out:         out,
		ResolveDir:  func() (string, error) { return dir, nil },
		FormatBytes: func(n int64) string { return "1B" },
	}
}

func TestRunListMissingDir(t *testing.T) {
	var out bytes.Buffer
	if err := RunList(params(&out, filepath.Join(t.TempDir(), "absent"), false)); err != nil {
		t.Fatalf("RunList: %v", err)
	}
	if !strings.Contains(out.String(), "does not exist") {
		t.Fatalf("want does-not-exist message, got %q", out.String())
	}
}

func TestRunListEmptyDir(t *testing.T) {
	var out bytes.Buffer
	if err := RunList(params(&out, t.TempDir(), false)); err != nil {
		t.Fatalf("RunList: %v", err)
	}
	if !strings.Contains(out.String(), "no backups in") {
		t.Fatalf("want no-backups message, got %q", out.String())
	}
}

func TestRunListFiltersAndTagsKinds(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "veska-backup-2026.tar.gz")
	writeFile(t, dir, "auto-pre-migration-2026.tar.gz")
	writeFile(t, dir, "unrelated.txt")
	writeFile(t, dir, "stray.tar.gz") // tar.gz but no recognized prefix

	var out bytes.Buffer
	if err := RunList(params(&out, dir, false)); err != nil {
		t.Fatalf("RunList: %v", err)
	}
	s := out.String()
	if !strings.Contains(s, "user") || !strings.Contains(s, "pre-migration") {
		t.Errorf("want both kinds tagged, got:\n%s", s)
	}
	if strings.Contains(s, "unrelated.txt") || strings.Contains(s, "stray.tar.gz") {
		t.Errorf("unrecognized files should be skipped, got:\n%s", s)
	}
}

func TestRunListJSONShape(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "veska-backup-2026.tar.gz")

	var out bytes.Buffer
	if err := RunList(params(&out, dir, true)); err != nil {
		t.Fatalf("RunList: %v", err)
	}
	var got struct {
		BackupDir string `json:"backup_dir"`
		Backups   []struct {
			Name string `json:"name"`
			Kind string `json:"kind"`
		} `json:"backups"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("invalid JSON: %v (raw=%s)", err, out.String())
	}
	if got.BackupDir != dir || len(got.Backups) != 1 || got.Backups[0].Kind != "user" {
		t.Fatalf("unexpected JSON: %+v", got)
	}
}

func TestRunListResolvesDirWhenUnset(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "veska-backup-2026.tar.gz")
	var out bytes.Buffer
	p := params(&out, "", false) // BackupDir empty → ResolveDir used
	p.ResolveDir = func() (string, error) { return dir, nil }
	if err := RunList(p); err != nil {
		t.Fatalf("RunList: %v", err)
	}
	if !strings.Contains(out.String(), "veska-backup-2026.tar.gz") {
		t.Fatalf("want resolved-dir listing, got %q", out.String())
	}
}
