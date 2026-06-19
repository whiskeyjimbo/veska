// SPDX-License-Identifier: AGPL-3.0-only

package depscmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// dep is a compact mirror of one eng_list_dependencies row, used to stage a
// fake daemon response.
type dep struct {
	Module       string         `json:"module"`
	Version      string         `json:"version,omitempty"`
	Language     string         `json:"language"`
	UsageCount   int            `json:"usage_count"`
	ImportCount  int            `json:"import_count,omitempty"`
	TopCallSites []callSiteShim `json:"top_call_sites"`
}

type callSiteShim struct {
	SrcNodeID  string `json:"src_node_id"`
	SymbolPath string `json:"symbol_path"`
}

// stubCall returns a CallFunc that decodes the given dependencies into
// RunList's response target via the same JSON path mcpclient.Call uses.
func stubCall(t *testing.T, deps []dep) CallFunc {
	t.Helper()
	return func(_ context.Context, method string, _ any, out any) error {
		if method != "eng_list_dependencies" {
			t.Fatalf("unexpected method %q", method)
		}
		b, err := json.Marshal(map[string]any{"dependencies": deps})
		if err != nil {
			t.Fatalf("marshal fake deps: %v", err)
		}
		if err := json.Unmarshal(b, out); err != nil {
			t.Fatalf("decode into Call target: %v", err)
		}
		return nil
	}
}

func baseParams(out *bytes.Buffer, call CallFunc) ListParams {
	return ListParams{
		RepoID: "r1", // skip the resolve paths; exercise rendering only
		Limit:  25,
		Out:    out,
		ErrOut: &bytes.Buffer{},
		Call:   call,
	}
}

func TestRunListNoDeps(t *testing.T) {
	var out bytes.Buffer
	if err := RunList(context.Background(), baseParams(&out, stubCall(t, nil))); err != nil {
		t.Fatalf("RunList: %v", err)
	}
	if !strings.Contains(out.String(), "no external dependencies") {
		t.Fatalf("want no-deps message, got %q", out.String())
	}
}

func TestRunListRendersTable(t *testing.T) {
	var out bytes.Buffer
	deps := []dep{
		{Module: "github.com/spf13/cobra", Version: "v1.8.0", Language: "go", UsageCount: 12, ImportCount: 3,
			TopCallSites: []callSiteShim{{SymbolPath: "cobra.Command"}, {SymbolPath: "cobra.ExactArgs"}}},
	}
	if err := RunList(context.Background(), baseParams(&out, stubCall(t, deps))); err != nil {
		t.Fatalf("RunList: %v", err)
	}
	s := out.String()
	for _, want := range []string{"MODULE", "github.com/spf13/cobra", "v1.8.0", "12", "cobra.Command, cobra.ExactArgs"} {
		if !strings.Contains(s, want) {
			t.Errorf("output missing %q:\n%s", want, s)
		}
	}
}

func TestRunListLimitTruncation(t *testing.T) {
	var out bytes.Buffer
	deps := make([]dep, 5)
	for i := range deps {
		deps[i] = dep{Module: "m" + string(rune('a'+i)), Language: "go", UsageCount: 1}
	}
	p := baseParams(&out, stubCall(t, deps))
	p.Limit = 2
	if err := RunList(context.Background(), p); err != nil {
		t.Fatalf("RunList: %v", err)
	}
	if !strings.Contains(out.String(), "... 3 more (raise --limit to see all)") {
		t.Fatalf("want truncation footer for 3 hidden rows, got %q", out.String())
	}
}

func TestRunListZeroCallsStarFooter(t *testing.T) {
	var out bytes.Buffer
	deps := []dep{
		{Module: "gopkg.in/yaml.v3", Version: "v3.0.1", Language: "go", UsageCount: 0, ImportCount: 4},
	}
	if err := RunList(context.Background(), baseParams(&out, stubCall(t, deps))); err != nil {
		t.Fatalf("RunList: %v", err)
	}
	s := out.String()
	if !strings.Contains(s, "0 *") {
		t.Errorf("want starred CALLS cell, got:\n%s", s)
	}
	if !strings.Contains(s, "chained_selectors_unresolved") {
		t.Errorf("want star-footer explanation, got:\n%s", s)
	}
}

func TestRunListJSONShape(t *testing.T) {
	var out bytes.Buffer
	deps := []dep{{Module: "github.com/foo/bar", Language: "go", UsageCount: 2, ImportCount: 1}}
	p := baseParams(&out, stubCall(t, deps))
	p.JSONOut = true
	if err := RunList(context.Background(), p); err != nil {
		t.Fatalf("RunList: %v", err)
	}
	var got struct {
		Dependencies []struct {
			Module     string `json:"module"`
			UsageCount int    `json:"usage_count"`
		} `json:"dependencies"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v (raw=%s)", err, out.String())
	}
	if len(got.Dependencies) != 1 || got.Dependencies[0].Module != "github.com/foo/bar" {
		t.Fatalf("unexpected JSON payload: %+v", got)
	}
}

func TestRunListPropagatesCallError(t *testing.T) {
	var out bytes.Buffer
	p := baseParams(&out, func(context.Context, string, any, any) error { return errors.New("daemon down") })
	err := RunList(context.Background(), p)
	if err == nil || !strings.Contains(err.Error(), "deps:") {
		t.Fatalf("want wrapped deps error, got %v", err)
	}
}

func TestSkippedSuffix(t *testing.T) {
	if got := skippedSuffix(0); got != "" {
		t.Errorf("skippedSuffix(0) = %q, want empty", got)
	}
	if got := skippedSuffix(3); !strings.Contains(got, "3 file(s) skipped") {
		t.Errorf("skippedSuffix(3) = %q", got)
	}
}
