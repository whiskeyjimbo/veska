// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

// Package logrotate provides a general-purpose rotating log writer that
// implements io.Writer. It rotates the active file when it reaches a
// configurable size limit and retains at most maxFiles rotated copies.
// Reopen supports SIGHUP-triggered log-file reopening.
package logrotate

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// RotatingWriter is a mutex-protected io.Writer that rotates the log file
// at a configurable byte limit and retains at most maxFiles rotated copies.
type RotatingWriter struct {
	mu          sync.Mutex
	path        string
	file        *os.File
	currentSize int64
	limitBytes  int64
	maxFiles    int
}

// NewRotatingWriter opens (or creates) the file at path, creating parent
// directories as needed, and returns a RotatingWriter. Rotation triggers
// when the file reaches limitBytes; at most maxFiles rotated copies are kept.
func NewRotatingWriter(path string, limitBytes int64, maxFiles int) (*RotatingWriter, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("logrotate.NewRotatingWriter: mkdir: %w", err)
	}
	f, size, err := openLogFile(path)
	if err != nil {
		return nil, fmt.Errorf("logrotate.NewRotatingWriter: %w", err)
	}
	return &RotatingWriter{
		path:        path,
		file:        f,
		currentSize: size,
		limitBytes:  limitBytes,
		maxFiles:    maxFiles,
	}, nil
}

// Write appends p to the log file. If adding p would meet or exceed the size
// limit the file is rotated first. Write is safe for concurrent use.
func (w *RotatingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.currentSize+int64(len(p)) >= w.limitBytes {
		if err := w.rotate(); err != nil {
			return 0, fmt.Errorf("logrotate.Write: rotate: %w", err)
		}
	}

	n, err := w.file.Write(p)
	w.currentSize += int64(n)
	if err != nil {
		return n, fmt.Errorf("logrotate.Write: %w", err)
	}
	return n, nil
}

// Reopen closes the current file and reopens it. This is intended for use
// with SIGHUP-based log rotation (e.g. after an external tool renames the
// file). Reopen is safe for concurrent use.
func (w *RotatingWriter) Reopen() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if err := w.file.Close(); err != nil {
		return fmt.Errorf("logrotate.Reopen: close: %w", err)
	}
	f, size, err := openLogFile(w.path)
	if err != nil {
		return fmt.Errorf("logrotate.Reopen: open: %w", err)
	}
	w.file = f
	w.currentSize = size
	return nil
}

// Close flushes and closes the underlying file.
func (w *RotatingWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.file.Sync(); err != nil {
		_ = w.file.Close()
		return fmt.Errorf("logrotate.Close: sync: %w", err)
	}
	if err := w.file.Close(); err != nil {
		return fmt.Errorf("logrotate.Close: %w", err)
	}
	return nil
}

// rotate renames rotated files upward (.{n-1}→.n, …,.1→.2) then renames the
// active file to.1 and opens a fresh active file. Callers must hold w.mu.
func (w *RotatingWriter) rotate() error {
	if err := w.file.Close(); err != nil {
		return fmt.Errorf("close active file: %w", err)
	}

	// Drop the oldest rotated copy.
	_ = os.Remove(rotatedName(w.path, w.maxFiles))

	// Shift existing rotated files upward.
	for i := w.maxFiles - 1; i >= 1; i-- {
		src := rotatedName(w.path, i)
		dst := rotatedName(w.path, i+1)
		if _, err := os.Stat(src); err == nil {
			if err := os.Rename(src, dst); err != nil {
				return fmt.Errorf("rename %s→%s: %w", src, dst, err)
			}
		}
	}

	// Rename the active file to.1.
	if err := os.Rename(w.path, rotatedName(w.path, 1)); err != nil {
		return fmt.Errorf("rename active to .1: %w", err)
	}

	// Open a fresh active file.
	f, _, err := openLogFile(w.path)
	if err != nil {
		return fmt.Errorf("open fresh file: %w", err)
	}
	w.file = f
	w.currentSize = 0
	return nil
}

// rotatedName returns the path for the nth rotated copy.
// e.g. rotatedName("/var/log/daemon.log", 1) → "/var/log/daemon.log.1"
func rotatedName(base string, n int) string {
	return fmt.Sprintf("%s.%d", base, n)
}

// openLogFile opens (or creates) the file at path in append mode and returns
// the file handle and the current file size.
func openLogFile(path string) (*os.File, int64, error) {
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
