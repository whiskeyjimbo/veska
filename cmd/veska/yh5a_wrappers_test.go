package main

import (
	"testing"

	"github.com/whiskeyjimbo/veska/internal/cli/graphcmd"
)

// TestBlastModeFromFlags pins the seed-selection logic added in:
// exactly one of {positional symbol, --dirty, --diff} must be chosen, and
// dirty/--diff reject a positional (they seed from changes, not a symbol).
func TestBlastModeFromFlags(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		dirty    bool
		diff     bool
		wantMode graphcmd.BlastMode
		wantSel  string
		wantRefA string
		wantRefB string
		wantErr  bool
	}{
		{name: "symbol", args: []string{"Server.Run"}, wantMode: graphcmd.BlastSymbol, wantSel: "Server.Run"},
		{name: "dirty", args: nil, dirty: true, wantMode: graphcmd.BlastDirty},
		{name: "diff working tree", args: nil, diff: true, wantMode: graphcmd.BlastDiff},
		{name: "diff range", args: []string{"main..HEAD"}, diff: true, wantMode: graphcmd.BlastDiff, wantRefA: "main", wantRefB: "HEAD"},
		{name: "diff range open right", args: []string{"main.."}, diff: true, wantMode: graphcmd.BlastDiff, wantRefA: "main", wantRefB: "HEAD"},
		{name: "diff bare ref", args: []string{"v1.2.0"}, diff: true, wantMode: graphcmd.BlastDiff, wantRefA: "v1.2.0", wantRefB: "HEAD"},
		{name: "diff range open left rejected", args: []string{"..HEAD"}, diff: true, wantErr: true},
		{name: "dirty and diff rejected", dirty: true, diff: true, wantErr: true},
		{name: "dirty with positional rejected", args: []string{"X"}, dirty: true, wantErr: true},
		{name: "no seed rejected", args: nil, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mode, sel, refA, refB, err := blastModeFromFlags(tt.args, tt.dirty, tt.diff)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got mode=%v sel=%q", mode, sel)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if mode != tt.wantMode {
				t.Fatalf("mode: want %v got %v", tt.wantMode, mode)
			}
			if sel != tt.wantSel {
				t.Fatalf("selector: want %q got %q", tt.wantSel, sel)
			}
			if refA != tt.wantRefA || refB != tt.wantRefB {
				t.Fatalf("refs: want (%q,%q) got (%q,%q)", tt.wantRefA, tt.wantRefB, refA, refB)
			}
		})
	}
}

// TestParseFileLine pins the `veska related <file:line>` anchor parsing.
func TestParseFileLine(t *testing.T) {
	tests := []struct {
		in       string
		wantPath string
		wantLine int
		wantErr  bool
	}{
		{in: "internal/foo.go:42", wantPath: "internal/foo.go", wantLine: 42},
		{in: "/abs/path/bar.go:1", wantPath: "/abs/path/bar.go", wantLine: 1},
		{in: "no-line.go", wantErr: true},
		{in: "trailing-colon.go:", wantErr: true},
		{in: "foo.go:0", wantErr: true},
		{in: "foo.go:abc", wantErr: true},
		{in: ":5", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			path, line, err := parseFileLine(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q, got path=%q line=%d", tt.in, path, line)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if path != tt.wantPath || line != tt.wantLine {
				t.Fatalf("want (%q,%d) got (%q,%d)", tt.wantPath, tt.wantLine, path, line)
			}
		})
	}
}

// TestYH5AWrappersRegistered asserts the new parity wrappers are wired onto
// the root command so they're reachable from the CLI (the other half of the
// cliparity contract — the manifest names a tool, the command must exist).
func TestYH5AWrappersRegistered(t *testing.T) {
	root := newRootCmd()
	want := []string{"node", "file-nodes", "similar", "related", "owner", "todos", "entry-points", "hot-zones"}
	have := map[string]bool{}
	for _, c := range root.Commands() {
		have[c.Name()] = true
	}
	for _, name := range want {
		if !have[name] {
			t.Errorf("expected top-level command %q to be registered", name)
		}
	}
	// repo show / repo current are subcommands of `repo`.
	for _, c := range root.Commands() {
		if c.Name() != "repo" {
			continue
		}
		sub := map[string]bool{}
		for _, s := range c.Commands() {
			sub[s.Name()] = true
		}
		for _, name := range []string{"show", "current"} {
			if !sub[name] {
				t.Errorf("expected `repo %s` subcommand to be registered", name)
			}
		}
	}
}
