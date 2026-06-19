// SPDX-License-Identifier: AGPL-3.0-only

package ownercmd

import "testing"

// TestLooksLikePath pins the symbol-vs-path routing fix from: a
// qualified Go symbol (FlagSet.Parse) contains a dot but must NOT be treated
// as a file path, or eng_find_owner git-blames a nonexistent file.
func TestLooksLikePath(t *testing.T) {
	paths := []string{"internal/foo.go", "flag.go", "a/b/c", "main.ts", "lib.rs"}
	symbols := []string{"FlagSet.Parse", "pkg.Func", "NewFlagSet", "Type.Method", "Parse"}
	for _, p := range paths {
		if !looksLikePath(p) {
			t.Errorf("expected %q to be treated as a path", p)
		}
	}
	for _, s := range symbols {
		if looksLikePath(s) {
			t.Errorf("expected %q to be treated as a symbol, not a path", s)
		}
	}
}
