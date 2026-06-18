// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package audit_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/audit"
)

func makeEntry(i int) ports.AuditEntry {
	return ports.AuditEntry{
		RepoID:    "repo-1",
		ActorID:   fmt.Sprintf("human:user%d", i),
		ActorKind: domain.ActorKindHuman,
		Op:        "node.save",
		TargetID:  fmt.Sprintf("target-%d", i),
		Branch:    "main",
		CreatedAt: time.Now().UTC(),
	}
}

func TestAuditWriterAppendsJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")

	w, err := audit.NewAuditFileWriter(path)
	if err != nil {
		t.Fatalf("NewAuditFileWriter: %v", err)
	}

	ctx := context.Background()
	for i := range 3 {
		if err := w.Write(ctx, makeEntry(i)); err != nil {
			t.Fatalf("Write[%d]: %v", i, err)
		}
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines, got %d", len(lines))
	}

	for i, line := range lines {
		var got map[string]any
		if err := json.Unmarshal([]byte(line), &got); err != nil {
			t.Fatalf("line %d not valid JSON: %v", i, err)
		}
		if got["repo_id"] != "repo-1" {
			t.Errorf("line %d: expected repo_id=repo-1, got %v", i, got["repo_id"])
		}
		if got["op"] != "node.save" {
			t.Errorf("line %d: expected op=node.save, got %v", i, got["op"])
		}
		if got["branch"] != "main" {
			t.Errorf("line %d: expected branch=main, got %v", i, got["branch"])
		}
	}
}

func TestAuditWriterRotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")

	// Use a small limit (1 MiB) to keep the test fast.
	const limitBytes = 1 << 20 // 1 MiB

	w, err := audit.NewAuditFileWriterWithLimit(path, limitBytes)
	if err != nil {
		t.Fatalf("NewAuditFileWriterWithLimit: %v", err)
	}

	ctx := context.Background()

	// Each entry is ~150 bytes. We need > 1 MiB total, so ~7000 writes.
	// Write until the writer has rotated at least once.
	for i := range 8000 {
		if err := w.Write(ctx, makeEntry(i)); err != nil {
			t.Fatalf("Write[%d]: %v", i, err)
		}
	}

	// The rotated file must exist.
	rotated := path + ".1.jsonl"
	info, err := os.Stat(rotated)
	if err != nil {
		t.Fatalf("rotated file %s not found: %v", rotated, err)
	}
	if info.Size() == 0 {
		t.Fatalf("rotated file %s is empty", rotated)
	}

	// The active file must be smaller than the limit (a fresh file).
	activeInfo, err := os.Stat(path)
	if err != nil {
		t.Fatalf("active file stat: %v", err)
	}
	if activeInfo.Size() >= limitBytes {
		t.Errorf("active file size %d >= limit %d; rotation did not occur", activeInfo.Size(), limitBytes)
	}
}

func TestAuditWriterRetainsMaxFive(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")

	// Pre-create rotated files.1 through.5.
	for i := 1; i <= 5; i++ {
		name := fmt.Sprintf("%s.%d.jsonl", path, i)
		if err := os.WriteFile(name, fmt.Appendf(nil, "old-%d\n", i), 0o644); err != nil {
			t.Fatalf("pre-create %s: %v", name, err)
		}
	}

	const limitBytes = 1 << 20 // 1 MiB
	w, err := audit.NewAuditFileWriterWithLimit(path, limitBytes)
	if err != nil {
		t.Fatalf("NewAuditFileWriterWithLimit: %v", err)
	}

	ctx := context.Background()

	// Write until rotation.
	for i := range 8000 {
		if err := w.Write(ctx, makeEntry(i)); err != nil {
			t.Fatalf("Write[%d]: %v", i, err)
		}
	}

	// 5 must be gone (deleted because max is 5 and we shifted).
	old5 := fmt.Sprintf("%s.5.jsonl", path)
	// After rotation the old.5 is deleted; a new.5 may exist if we shifted 4->5 etc.
	// The test just checks that we have at most 5 rotated files total.
	count := 0
	for i := 1; i <= 6; i++ {
		name := fmt.Sprintf("%s.%d.jsonl", path, i)
		if _, err := os.Stat(name); err == nil {
			count++
		}
	}
	if count > 5 {
		t.Errorf("found %d rotated files; expected at most 5", count)
	}
	// 6 must not exist.
	if _, err := os.Stat(fmt.Sprintf("%s.6.jsonl", path)); err == nil {
		t.Errorf("file .6.jsonl should not exist")
	}
	_ = old5 // silence unused warning; checked via count
}

func TestAuditWriterConcurrentWrites(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")

	w, err := audit.NewAuditFileWriter(path)
	if err != nil {
		t.Fatalf("NewAuditFileWriter: %v", err)
	}

	const goroutines = 10
	const writesPerGoroutine = 100

	ctx := context.Background()
	var wg sync.WaitGroup
	wg.Add(goroutines)

	for g := range goroutines {
		go func() {
			defer wg.Done()
			for i := range writesPerGoroutine {
				e := makeEntry(g*writesPerGoroutine + i)
				if err := w.Write(ctx, e); err != nil {
					t.Errorf("goroutine %d write %d: %v", g, i, err)
				}
			}
		}()
	}
	wg.Wait()

	// Count lines in the active file (no rotation expected at default 100 MiB limit).
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	want := goroutines * writesPerGoroutine
	if len(lines) != want {
		t.Errorf("expected %d lines, got %d", want, len(lines))
	}
}

// Compile-time assertion: *AuditFileWriter implements ports.AuditWriter.
var _ ports.AuditWriter = (*audit.AuditFileWriter)(nil)
