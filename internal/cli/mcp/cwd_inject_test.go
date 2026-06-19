// SPDX-License-Identifier: AGPL-3.0-only

package mcp

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"
)

// verification - the shim rewrites eng_get_current_repo
// requests that omit cwd to carry the shim's working directory.

func TestMaybeInjectCwd_AddsCwdWhenMissing(t *testing.T) {
	in := []byte(`{"jsonrpc":"2.0","id":1,"method":"eng_get_current_repo","params":{}}` + "\n")
	out, ok := maybeInjectCwd(in, "/abs/work")
	if !ok {
		t.Fatalf("expected rewrite, got pass-through")
	}
	var msg map[string]any
	if err := json.Unmarshal(out, &msg); err != nil {
		t.Fatalf("rewritten frame not valid JSON: %v", err)
	}
	p, _ := msg["params"].(map[string]any)
	if cwd, _ := p["cwd"].(string); cwd != "/abs/work" {
		t.Fatalf("expected cwd=/abs/work, got %q", cwd)
	}
	if !bytes.HasSuffix(out, []byte("\n")) {
		t.Fatalf("rewritten frame must keep trailing newline; got %q", out)
	}
}

func TestMaybeInjectCwd_KeepsExplicitCwd(t *testing.T) {
	in := []byte(`{"jsonrpc":"2.0","id":1,"method":"eng_get_current_repo","params":{"cwd":"/explicit"}}` + "\n")
	_, ok := maybeInjectCwd(in, "/abs/work")
	if ok {
		t.Fatal("must not rewrite a frame that already carries cwd")
	}
}

// TestMaybeInjectCwd_AllEngMethodsRewritten guards: every eng_*
// call gets cwd injected (when missing) so the daemon can fall back to it
// when repo_id is omitted. Non-eng_* methods (and frames that already carry
// cwd) still pass through unchanged.
func TestMaybeInjectCwd_AllEngMethodsRewritten(t *testing.T) {
	cases := []string{
		"eng_find_symbol", "eng_get_node", "eng_search_semantic",
		"eng_list_repos", "eng_get_status",
	}
	for _, m := range cases {
		t.Run(m, func(t *testing.T) {
			in := []byte(`{"jsonrpc":"2.0","id":1,"method":"` + m + `","params":{}}` + "\n")
			out, ok := maybeInjectCwd(in, "/abs/work")
			if !ok {
				t.Fatalf("expected rewrite for %s", m)
			}
			var msg map[string]any
			_ = json.Unmarshal(out, &msg)
			p, _ := msg["params"].(map[string]any)
			if cwd, _ := p["cwd"].(string); cwd != "/abs/work" {
				t.Fatalf("expected cwd=/abs/work, got %q", cwd)
			}
		})
	}
}

func TestMaybeInjectCwd_OtherMethodsPassThrough(t *testing.T) {
	// Non-eng_* methods (tools/list, etc.) are not rewritten.
	in := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}` + "\n")
	_, ok := maybeInjectCwd(in, "/abs/work")
	if ok {
		t.Fatal("non-eng_* methods should not be rewritten")
	}
}

func TestMaybeInjectCwd_NonJSONPassesThrough(t *testing.T) {
	in := []byte("not json at all\n")
	_, ok := maybeInjectCwd(in, "/abs/work")
	if ok {
		t.Fatal("non-JSON frames must pass through unchanged")
	}
}

func TestInjectCwdAndCopy_StreamRewritesOnlyTargetFrames(t *testing.T) {
	src := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"eng_list_repos","params":{}}`,
		`{"jsonrpc":"2.0","id":2,"method":"eng_get_current_repo","params":{}}`,
		`{"jsonrpc":"2.0","id":3,"method":"eng_find_symbol","params":{"symbol":"X"}}`,
		"",
	}, "\n")
	var dst bytes.Buffer
	// Use a small custom os.Getwd by setting cwd through the helper.
	// injectCwdAndCopy reads os.Getwd internally; we just verify the rewrite
	// for frame 2 by checking the output contains a non-empty cwd value.
	injectCwdAndCopy(&dst, io.NopCloser(strings.NewReader(src)))
	out := dst.String()
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 frames out, got %d:\n%s", len(lines), out)
	}
	var frame2 map[string]any
	if err := json.Unmarshal([]byte(lines[1]), &frame2); err != nil {
		t.Fatalf("frame 2 not valid JSON: %v", err)
	}
	p, _ := frame2["params"].(map[string]any)
	if cwd, _ := p["cwd"].(string); cwd == "" {
		t.Fatalf("eng_get_current_repo frame should now carry cwd, got: %s", lines[1])
	}
	// Frames 1 and 3 are also eng_* calls - they should now carry cwd too
	// ( broadened the rewrite from just eng_get_current_repo to
	// every eng_* method).
	var frame1, frame3 map[string]any
	_ = json.Unmarshal([]byte(lines[0]), &frame1)
	_ = json.Unmarshal([]byte(lines[2]), &frame3)
	if cwd, _ := frame1["params"].(map[string]any)["cwd"].(string); cwd == "" {
		t.Fatalf("eng_list_repos frame should also carry cwd post-ktz0: %s", lines[0])
	}
	if cwd, _ := frame3["params"].(map[string]any)["cwd"].(string); cwd == "" {
		t.Fatalf("eng_find_symbol frame should also carry cwd post-ktz0: %s", lines[2])
	}
}
