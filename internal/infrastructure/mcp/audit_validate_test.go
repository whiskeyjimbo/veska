package mcp

import (
	"testing"
	"time"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// TestValidateAuditEntry covers all required-field invariants.
func TestValidateAuditEntry(t *testing.T) {
	good := ports.AuditEntry{
		RepoID:    "repo-1",
		ActorID:   "service:veska",
		ActorKind: domain.ActorKindSystem,
		Op:        "node.save",
		TargetID:  "target-x",
		Branch:    "main",
		CreatedAt: time.Now(),
	}

	tests := []struct {
		name    string
		mutate  func(e *ports.AuditEntry)
		wantErr bool
	}{
		{
			name:    "valid entry passes",
			mutate:  func(_ *ports.AuditEntry) {},
			wantErr: false,
		},
		{
			name:    "empty ActorID rejected",
			mutate:  func(e *ports.AuditEntry) { e.ActorID = "" },
			wantErr: true,
		},
		{
			name:    "empty ActorKind rejected",
			mutate:  func(e *ports.AuditEntry) { e.ActorKind = "" },
			wantErr: true,
		},
		{
			name:    "invalid ActorKind rejected",
			mutate:  func(e *ports.AuditEntry) { e.ActorKind = "robot" },
			wantErr: true,
		},
		{
			name:    "empty Op rejected",
			mutate:  func(e *ports.AuditEntry) { e.Op = "" },
			wantErr: true,
		},
		{
			name:    "empty RepoID rejected",
			mutate:  func(e *ports.AuditEntry) { e.RepoID = "" },
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := good // copy
			tc.mutate(&e)
			err := ValidateAuditEntry(e)
			if tc.wantErr && err == nil {
				t.Error("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}
