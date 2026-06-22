// SPDX-License-Identifier: AGPL-3.0-only

package fts

import (
	"context"
	"errors"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

type fakeReindexer struct {
	calls   []string // "repo/branch/path"
	failErr error
}

func (f *fakeReindexer) ReindexFile(_ context.Context, repoID, branch, filePath string) error {
	f.calls = append(f.calls, repoID+"/"+branch+"/"+filePath)
	return f.failErr
}

func TestNewHandler_NilRepo(t *testing.T) {
	if _, err := NewHandler(nil); !errors.Is(err, ErrMissingDependency) {
		t.Fatalf("NewHandler(nil) err = %v, want ErrMissingDependency", err)
	}
}

func TestHandle_ReindexesPayloadFile(t *testing.T) {
	fr := &fakeReindexer{}
	h, err := NewHandler(fr)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	row := ports.WorkRow{Kind: ports.WorkKindFTS, RepoID: "r1", Branch: "main", Payload: "src/a.go"}
	if err := h.Handle(context.Background(), row); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(fr.calls) != 1 || fr.calls[0] != "r1/main/src/a.go" {
		t.Fatalf("reindex calls = %v, want [r1/main/src/a.go]", fr.calls)
	}
}

func TestHandle_EmptyPayloadIsNoop(t *testing.T) {
	fr := &fakeReindexer{}
	h, _ := NewHandler(fr)
	if err := h.Handle(context.Background(), ports.WorkRow{Kind: ports.WorkKindFTS}); err != nil {
		t.Fatalf("Handle empty payload: %v", err)
	}
	if len(fr.calls) != 0 {
		t.Fatalf("expected no reindex on empty payload, got %v", fr.calls)
	}
}

func TestHandle_WrongKindErrors(t *testing.T) {
	h, _ := NewHandler(&fakeReindexer{})
	if err := h.Handle(context.Background(), ports.WorkRow{Kind: ports.WorkKindAutoLink, Payload: "x"}); err == nil {
		t.Fatal("Handle with wrong kind: want error, got nil")
	}
}
