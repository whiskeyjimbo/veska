// SPDX-License-Identifier: AGPL-3.0-only

//go:build eval

package recall

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"

	"github.com/whiskeyjimbo/veska/internal/core/ports"
	"github.com/whiskeyjimbo/veska/tools/loadtest/synthcorpus"
)

// ProgressFunc is the optional callback invoked by GenerateOllamaFixture
// after each successful embed. done is the number of vectors written so
// far (1-indexed at the boundary call); total is len(nodes). Callers
// typically throttle their own output by checking done%N == 0.
type ProgressFunc func(done, total int)

// GenerateOllamaFixture drives provider over every node in corpus,
// writing the resulting vectors to dst in the on-disk fixture format
// understood by ReadFixture. The file is written atomically: vectors
// accumulate in memory and a single temp-file → rename hop publishes
// the canonical path so a Ctrl-C or provider failure mid-run leaves no
// half-written artefact behind.
// The first embed call doubles as the dim-detection step; subsequent
// embeds whose length disagrees are reported as an error rather than
// silently truncated.
// Context cancellation propagates: the loop exits as soon as ctx is
// Done and the caller receives a wrapped ctx.Err.
func GenerateOllamaFixture(
	ctx context.Context,
	provider ports.EmbeddingProvider,
	nodes []synthcorpus.SyntheticNode,
	dst string,
	progress ProgressFunc,
) error {
	if provider == nil {
		return errors.New("recall: GenerateOllamaFixture: nil provider")
	}
	if len(nodes) == 0 {
		return errors.New("recall: GenerateOllamaFixture: empty corpus")
	}
	if dst == "" {
		return errors.New("recall: GenerateOllamaFixture: empty dst path")
	}

	var (
		dim     int
		vectors []float32
	)

	for i, n := range nodes {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("recall: GenerateOllamaFixture: %w", err)
		}
		vec, err := provider.Embed(ctx, n.Text)
		if err != nil {
			return fmt.Errorf("recall: GenerateOllamaFixture: embed node %d (%s): %w", i, n.NodeID, err)
		}
		if i == 0 {
			dim = len(vec)
			if dim <= 0 {
				return fmt.Errorf("recall: GenerateOllamaFixture: provider returned dim=%d for node 0", dim)
			}
			vectors = make([]float32, 0, dim*len(nodes))
		} else if len(vec) != dim {
			return fmt.Errorf("recall: GenerateOllamaFixture: dim drift at node %d: got %d want %d",
				i, len(vec), dim)
		}
		// L2-normalize before storing. Embedding models such as
		// nomic-embed-text return vectors with norm far from 1.0 (~19);
		// the auto-link score = 1/(1+L2dist) only lands in its documented
		// [0,1] threshold range for unit vectors. Production must do the
		// same in the embedder pipeline.
		l2NormalizeInPlace(vec)
		vectors = append(vectors, vec...)
		if progress != nil {
			progress(i+1, len(nodes))
		}
	}

	return writeFixtureAtomic(dst, dim, vectors)
}

// l2NormalizeInPlace scales v to unit L2 norm. A zero vector is left
// unchanged (no division by zero).
func l2NormalizeInPlace(v []float32) {
	var sq float64
	for _, x := range v {
		sq += float64(x) * float64(x)
	}
	if sq == 0 {
		return
	}
	inv := float32(1.0 / math.Sqrt(sq))
	for i := range v {
		v[i] *= inv
	}
}

// writeFixtureAtomic writes to <dst>.tmp-<pid> then renames into place.
// The temp file is created in the same directory as dst so the rename
// stays on a single filesystem.
func writeFixtureAtomic(dst string, dim int, vectors []float32) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("recall: GenerateOllamaFixture: mkdir: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), filepath.Base(dst)+".tmp-*")
	if err != nil {
		return fmt.Errorf("recall: GenerateOllamaFixture: tempfile: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpPath) }

	hdr := FixtureHeader{Dim: uint32(dim), Count: uint32(len(vectors) / dim)}
	if err := binary.Write(tmp, binary.LittleEndian, hdr); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("recall: GenerateOllamaFixture: write header: %w", err)
	}
	if err := binary.Write(tmp, binary.LittleEndian, vectors); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("recall: GenerateOllamaFixture: write body: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("recall: GenerateOllamaFixture: fsync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("recall: GenerateOllamaFixture: close: %w", err)
	}
	if err := os.Rename(tmpPath, dst); err != nil {
		cleanup()
		return fmt.Errorf("recall: GenerateOllamaFixture: rename: %w", err)
	}
	return nil
}
