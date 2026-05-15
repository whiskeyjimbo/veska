package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"
)

// ── SourceLayer tests ──────────────────────────────────────────────────────

func TestSourceLayer_ValidValues(t *testing.T) {
	layers := []SourceLayer{
		LayerStructural,
		LayerSemantic,
		LayerSecurity,
		LayerQuality,
	}
	for _, l := range layers {
		if l == "" {
			t.Errorf("SourceLayer constant must not be empty")
		}
	}
}

// ── Severity tests ─────────────────────────────────────────────────────────

func TestSeverity_AtLeast(t *testing.T) {
	cases := []struct {
		a, b SourceLayer
		// re-use string fields for severity
	}{}
	_ = cases

	if !SeverityHigh.AtLeast(SeverityMedium) {
		t.Error("high should be at least medium")
	}
	if !SeverityCritical.AtLeast(SeverityCritical) {
		t.Error("critical should be at least critical")
	}
	if SeverityLow.AtLeast(SeverityMedium) {
		t.Error("low should NOT be at least medium")
	}
	if SeverityInfo.AtLeast(SeverityLow) {
		t.Error("info should NOT be at least low")
	}

	// ordering: info < low < medium < high < critical
	ordered := []Severity{SeverityInfo, SeverityLow, SeverityMedium, SeverityHigh, SeverityCritical}
	for i := 1; i < len(ordered); i++ {
		if !ordered[i].AtLeast(ordered[i-1]) {
			t.Errorf("%s should be >= %s", ordered[i], ordered[i-1])
		}
		if ordered[i-1].AtLeast(ordered[i]) {
			t.Errorf("%s should NOT be >= %s", ordered[i-1], ordered[i])
		}
	}
}

// ── Finding constructor tests ──────────────────────────────────────────────

func expectedFindingID(rule, anchor string) string {
	h := sha256.Sum256([]byte(rule + "\x00" + anchor))
	return hex.EncodeToString(h[:])[:32]
}

func TestNewFinding_NodeAnchor(t *testing.T) {
	f, err := NewFinding("ulid-1", "repo-a", "main",
		SeverityMedium, LayerStructural,
		"no-unused-exports", "symbol X is unused",
		WithNodeAnchor("node-abc"),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if f.ID != "ulid-1" {
		t.Errorf("id mismatch: got %q", f.ID)
	}
	if f.RepoID != "repo-a" {
		t.Errorf("repo_id mismatch")
	}
	if f.Branch != "main" {
		t.Errorf("branch mismatch")
	}
	if f.Rule != "no-unused-exports" {
		t.Errorf("rule mismatch")
	}
	if f.State != FindingStateOpen {
		t.Errorf("default state should be open")
	}
	if f.NodeID == nil || *f.NodeID != "node-abc" {
		t.Errorf("node_id not set correctly")
	}
	if f.FilePath != nil {
		t.Errorf("file_path should be nil when node anchor used")
	}
	want := expectedFindingID("no-unused-exports", "node-abc")
	if f.FindingID != want {
		t.Errorf("finding_id: got %q want %q", f.FindingID, want)
	}
}

func TestNewFinding_FileAnchor(t *testing.T) {
	f, err := NewFinding("ulid-2", "repo-b", "dev",
		SeverityHigh, LayerSecurity,
		"sql-injection", "potential SQL injection",
		WithFileAnchor("pkg/db/query.go"),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if f.FilePath == nil || *f.FilePath != "pkg/db/query.go" {
		t.Errorf("file_path not set correctly")
	}
	if f.NodeID != nil {
		t.Errorf("node_id should be nil when file anchor used")
	}
	want := expectedFindingID("sql-injection", "pkg/db/query.go")
	if f.FindingID != want {
		t.Errorf("finding_id: got %q want %q", f.FindingID, want)
	}
}

func TestNewFinding_StableFindingID_AcrossBranches(t *testing.T) {
	f1, _ := NewFinding("u1", "repo", "main", SeverityLow, LayerQuality,
		"rule-x", "msg", WithNodeAnchor("n1"))
	f2, _ := NewFinding("u2", "repo", "feature-branch", SeverityLow, LayerQuality,
		"rule-x", "msg", WithNodeAnchor("n1"))
	if f1.FindingID != f2.FindingID {
		t.Errorf("finding_id must be stable across branches: %q != %q", f1.FindingID, f2.FindingID)
	}
}

func TestNewFinding_ErrorNoAnchor(t *testing.T) {
	_, err := NewFinding("u1", "repo", "main",
		SeverityInfo, LayerSemantic,
		"some-rule", "message")
	if err == nil {
		t.Error("expected error when no anchor provided")
	}
}

func TestNewFinding_ErrorEmptyRule(t *testing.T) {
	_, err := NewFinding("u1", "repo", "main",
		SeverityInfo, LayerSemantic,
		"", "message",
		WithNodeAnchor("node-1"))
	if err == nil {
		t.Error("expected error when rule is empty")
	}
}

func TestNewFinding_ErrorInvalidSeverity(t *testing.T) {
	_, err := NewFinding("u1", "repo", "main",
		Severity("bogus"), LayerStructural,
		"rule", "msg",
		WithNodeAnchor("node-1"))
	if err == nil {
		t.Error("expected error for invalid severity")
	}
}

func TestNewFinding_ErrorInvalidSourceLayer(t *testing.T) {
	_, err := NewFinding("u1", "repo", "main",
		SeverityInfo, SourceLayer("bogus"),
		"rule", "msg",
		WithNodeAnchor("node-1"))
	if err == nil {
		t.Error("expected error for invalid source layer")
	}
}

func TestNewFinding_OpenStateHasNilClosedFields(t *testing.T) {
	f, err := NewFinding("u1", "repo", "main",
		SeverityMedium, LayerQuality,
		"rule", "msg",
		WithNodeAnchor("n1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if f.ClosedAt != nil {
		t.Error("closed_at must be nil for open finding")
	}
	if f.ClosedReason != nil {
		t.Error("closed_reason must be nil for open finding")
	}
}

// ── WithAnchorContentHash tests ────────────────────────────────────────────

func TestWithAnchorContentHash_Sets(t *testing.T) {
	f, err := NewFinding("u1", "repo", "main",
		SeverityLow, LayerStructural, "dead-code", "msg",
		WithNodeAnchor("n1"),
		WithAnchorContentHash("h-abc123"),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if f.AnchorContentHash == nil {
		t.Fatal("AnchorContentHash is nil")
	}
	if *f.AnchorContentHash != "h-abc123" {
		t.Errorf("AnchorContentHash = %q, want h-abc123", *f.AnchorContentHash)
	}
}

func TestWithAnchorContentHash_DefaultNil(t *testing.T) {
	f, err := NewFinding("u1", "repo", "main",
		SeverityLow, LayerStructural, "parse-failure", "msg",
		WithFileAnchor("foo.go"),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if f.AnchorContentHash != nil {
		t.Errorf("AnchorContentHash should default to nil, got %v", *f.AnchorContentHash)
	}
}

func TestWithAnchorContentHash_EmptyErrors(t *testing.T) {
	_, err := NewFinding("u1", "repo", "main",
		SeverityLow, LayerStructural, "dead-code", "msg",
		WithNodeAnchor("n1"),
		WithAnchorContentHash(""),
	)
	if err == nil {
		t.Error("expected error for empty content hash")
	}
}

// ── Finding.Close tests ────────────────────────────────────────────────────

func TestFinding_Close_Low_AnyActor(t *testing.T) {
	f, _ := NewFinding("u1", "repo", "main",
		SeverityLow, LayerQuality, "rule", "msg",
		WithNodeAnchor("n1"))
	now := time.Now()
	err := f.Close("fixed", string(ActorKindAgent), "agent-007", now)
	if err != nil {
		t.Fatalf("unexpected error closing low finding: %v", err)
	}
	if f.State != FindingStateClosed {
		t.Error("state should be closed after Close()")
	}
	if f.ClosedAt == nil || !f.ClosedAt.Equal(now) {
		t.Error("closed_at not set correctly")
	}
	if f.ClosedReason == nil || *f.ClosedReason != "fixed" {
		t.Error("closed_reason not set correctly")
	}
}

func TestFinding_Close_High_RequiresHuman(t *testing.T) {
	f, _ := NewFinding("u1", "repo", "main",
		SeverityHigh, LayerSecurity, "rule", "msg",
		WithNodeAnchor("n1"))
	now := time.Now()
	err := f.Close("fixed", string(ActorKindAgent), "agent-007", now)
	if err == nil {
		t.Error("expected error: high severity requires human actor to close")
	}
}

func TestFinding_Close_Critical_RequiresHuman(t *testing.T) {
	f, _ := NewFinding("u1", "repo", "main",
		SeverityCritical, LayerSecurity, "rule", "msg",
		WithNodeAnchor("n1"))
	now := time.Now()
	err := f.Close("fixed", string(ActorKindHuman), "human-1", now)
	if err != nil {
		t.Fatalf("human can close critical: %v", err)
	}
	if f.State != FindingStateClosed {
		t.Error("state should be closed")
	}
}

func TestFinding_Close_AlreadyClosed(t *testing.T) {
	f, _ := NewFinding("u1", "repo", "main",
		SeverityLow, LayerQuality, "rule", "msg",
		WithNodeAnchor("n1"))
	now := time.Now()
	_ = f.Close("fixed", string(ActorKindHuman), "h1", now)
	err := f.Close("again", string(ActorKindHuman), "h1", now)
	if err == nil {
		t.Error("expected error closing already-closed finding")
	}
}
