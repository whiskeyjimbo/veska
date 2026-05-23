package mcp

import "testing"

// TestCheckRequired_ReportsAllMissing pins solov2-d2x: a call missing several
// required params learns all of them from one error, not one at a time.
func TestCheckRequired_ReportsAllMissing(t *testing.T) {
	t.Run("all present", func(t *testing.T) {
		if err := checkRequired("repo_id", "r", "branch", "main"); err != nil {
			t.Fatalf("expected nil, got %+v", err)
		}
	})

	t.Run("single missing names it", func(t *testing.T) {
		err := checkRequired("repo_id", "", "branch", "main")
		if err == nil || err.Code != CodeInvalidParams {
			t.Fatalf("expected InvalidParams, got %+v", err)
		}
		if err.Message != "repo_id is required" {
			t.Errorf("got %q", err.Message)
		}
	})

	t.Run("multiple missing lists all", func(t *testing.T) {
		err := checkRequired("query", "", "repo_id", "", "branch", "main")
		if err == nil {
			t.Fatal("expected error")
			return
		}
		if err.Message != "missing required params: query, repo_id" {
			t.Errorf("expected both names in one message, got %q", err.Message)
		}
	})
}
