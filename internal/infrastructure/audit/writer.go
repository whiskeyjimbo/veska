// Package audit provides an append-only JSONL file writer that implements
// the ports.AuditWriter port. The writer rotates the log file when it reaches
// a configurable size limit (default 100 MiB) and retains at most 5 rotated files.
package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"

	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

const (
	// defaultSizeLimit is 100 MiB.
	defaultSizeLimit int64 = 100 << 20

	maxRotatedFiles = 5
)

// auditRecord is the on-disk JSON representation of a single AuditEntry.
type auditRecord struct {
	RepoID    string `json:"repo_id"`
	ActorID   string `json:"actor_id"`
	ActorKind string `json:"actor_kind"`
	Op        string `json:"op"`
	TargetID  string `json:"target_id"`
	Branch    string `json:"branch"`
	CreatedAt string `json:"created_at"`
	Reason    string `json:"reason,omitempty"`
}

// AuditFileWriter is a synchronous, mutex-protected JSONL file writer that
// rotates the log when the file exceeds a size limit and retains at most 5
// rotated files.
type AuditFileWriter struct {
	mu          sync.Mutex
	path        string
	file        *os.File
	currentSize int64
	sizeLimit   int64
}

// NewAuditFileWriter creates or opens the file at path and returns an
// AuditFileWriter using the default 100 MiB rotation limit.
func NewAuditFileWriter(path string) (*AuditFileWriter, error) {
	return NewAuditFileWriterWithLimit(path, defaultSizeLimit)
}

// NewAuditFileWriterWithLimit creates or opens the log file using the specified
// limit. Use this constructor in tests to keep test data small.
func NewAuditFileWriterWithLimit(path string, limitBytes int64) (*AuditFileWriter, error) {
	f, size, err := openAuditFile(path)
	if err != nil {
		return nil, fmt.Errorf("audit.NewAuditFileWriterWithLimit: %w", err)
	}
	return &AuditFileWriter{
		path:        path,
		file:        f,
		currentSize: size,
		sizeLimit:   limitBytes,
	}, nil
}

// Write serializes e as a single JSON line and appends it to the log file.
// Write is safe for concurrent use.
func (w *AuditFileWriter) Write(_ context.Context, e ports.AuditEntry) error {
	rec := auditRecord{
		RepoID:    e.RepoID,
		ActorID:   e.ActorID,
		ActorKind: string(e.ActorKind),
		Op:        e.Op,
		TargetID:  e.TargetID,
		Branch:    e.Branch,
		CreatedAt: e.CreatedAt.UTC().Format("2006-01-02T15:04:05.999999999Z"),
		Reason:    e.Reason,
	}

	line, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("audit.Write: marshal: %w", err)
	}
	line = append(line, '\n')

	w.mu.Lock()
	defer w.mu.Unlock()

	// Rotate before writing if adding this line would meet or exceed the limit.
	if w.currentSize+int64(len(line)) >= w.sizeLimit {
		if err := w.rotate(); err != nil {
			return fmt.Errorf("audit.Write: rotate: %w", err)
		}
	}

	n, err := w.file.Write(line)
	if err != nil {
		return fmt.Errorf("audit.Write: %w", err)
	}
	w.currentSize += int64(n)
	return nil
}

// rotate renames existing rotated files upward (.4 to .5, .3 to .4, etc.), renames
// the active log to .1.jsonl, and opens a fresh active file. Callers must hold w.mu.
func (w *AuditFileWriter) rotate() error {
	// Close the current file before renaming it.
	if err := w.file.Close(); err != nil {
		return fmt.Errorf("close active file: %w", err)
	}

	// Delete the oldest rotated file if it exists (.5).
	oldest := rotatedName(w.path, maxRotatedFiles)
	_ = os.Remove(oldest)

	// Shift existing rotated files: .4 to .5, .3 to .4, etc.
	for i := maxRotatedFiles - 1; i >= 1; i-- {
		src := rotatedName(w.path, i)
		dst := rotatedName(w.path, i+1)
		if _, err := os.Stat(src); err == nil {
			if err := os.Rename(src, dst); err != nil {
				return fmt.Errorf("rename %s to %s: %w", src, dst, err)
			}
		}
	}

	// Rename the active file to .1.
	if err := os.Rename(w.path, rotatedName(w.path, 1)); err != nil {
		return fmt.Errorf("rename active to .1: %w", err)
	}

	// Open a fresh active file.
	f, _, err := openAuditFile(w.path)
	if err != nil {
		return fmt.Errorf("open fresh file: %w", err)
	}
	w.file = f
	w.currentSize = 0
	return nil
}

func rotatedName(base string, n int) string {
	return fmt.Sprintf("%s.%d.jsonl", base, n)
}

// openAuditFile opens (or creates) the file at path in append mode and returns
// the file and its current size.
func openAuditFile(path string) (*os.File, int64, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, 0, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, 0, err
	}
	return f, info.Size(), nil
}
