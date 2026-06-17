// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package mcpclient

import (
	"errors"
	"testing"
)

func TestInjectCwd(t *testing.T) {
	t.Run("non-eng method passes through unchanged", func(t *testing.T) {
		in := map[string]any{"a": 1}
		out := injectCwd("tools/list", in)
		if m, ok := out.(map[string]any); !ok || m["cwd"] != nil {
			t.Fatalf("expected no cwd on non-eng_ method, got %v", out)
		}
	})

	t.Run("eng method gets cwd injected", func(t *testing.T) {
		out, ok := injectCwd("eng_get_node", map[string]any{}).(map[string]any)
		if !ok {
			t.Fatalf("expected map result, got %T", out)
		}
		if s, _ := out["cwd"].(string); s == "" {
			t.Fatal("expected cwd to be injected for eng_ method")
		}
	})

	t.Run("skip-list method does not get cwd", func(t *testing.T) {
		out := injectCwd("eng_find_symbol", map[string]any{})
		if m, _ := out.(map[string]any); m["cwd"] != nil {
			t.Fatalf("eng_find_symbol is in methodsSkipCwd; cwd must not be injected, got %v", m)
		}
	})

	t.Run("existing cwd is preserved", func(t *testing.T) {
		out, _ := injectCwd("eng_get_node", map[string]any{"cwd": "/explicit"}).(map[string]any)
		if out["cwd"] != "/explicit" {
			t.Fatalf("explicit cwd must be preserved, got %v", out["cwd"])
		}
	})
}

func TestHumanizeError(t *testing.T) {
	got := humanizeError("not found; pass eng_list_repos to find the id")
	if got != "not found; run `veska repo list` to see ids" {
		t.Fatalf("eng_ hint not humanized: %q", got)
	}
}

func TestIsDaemonUnreachable(t *testing.T) {
	if IsDaemonUnreachable(nil) {
		t.Fatal("nil error must not be unreachable")
	}
	if !IsDaemonUnreachable(errors.New("dial /x: daemon not running")) {
		t.Fatal("daemon-not-running must be unreachable")
	}
	if IsDaemonUnreachable(errors.New("daemon: human_required")) {
		t.Fatal("a real call error must not be classed as unreachable")
	}
}
