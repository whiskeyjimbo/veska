// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

// Package archive provides small, dependency-free helpers for inspecting
// archive files. It is a zero-dep platform leaf, importable from both the
// infrastructure backup writer and the doctor diagnostic consumer without
// crossing a layering boundary.
package archive

import (
	"compress/gzip"
	"io"
	"os"
)

// VerifyGzip opens path as a gzip stream and reads at least the first byte,
// confirming the archive is readable. Returns nil on success, or an error
// describing the failure.
func VerifyGzip(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	gr, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gr.Close()

	// Read at least one byte to confirm the stream is readable.
	buf := make([]byte, 1)
	_, err = gr.Read(buf)
	if err != nil && err != io.EOF {
		return err
	}
	return nil
}
