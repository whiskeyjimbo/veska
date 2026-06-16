//go:build eval

package recall

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/whiskeyjimbo/veska/tools/loadtest/synthcorpus"
)

// stubProvider is a deterministic in-memory EmbeddingProvider used to
// exercise the generator end-to-end without hitting real Ollama. Each
// text deterministically maps to a vector of length dim where the first
// byte of text drives the spike — enough to assert the body is round
// tripped and the dim header is correct.
type stubProvider struct {
	dim    int
	calls  int
	mu     sync.Mutex
	failOn int // 1-indexed call number that should return an error; 0 disables
}

func (s *stubProvider) ModelID() string { return "stub-v1" }

func (s *stubProvider) Embed(ctx context.Context, text string) ([]float32, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.calls++
	n := s.calls
	s.mu.Unlock()
	if s.failOn > 0 && n == s.failOn {
		return nil, errors.New("stub: induced failure")
	}
	v := make([]float32, s.dim)
	if len(text) > 0 {
		v[0] = float32(text[0])
	}
	v[1] = float32(n)
	return v, nil
}

func TestGenerateOllamaFixture_WritesRoundTrippableFile(t *testing.T) {
	corpus := synthcorpus.GenerateCorpus(4, 3) // 12 nodes
	prov := &stubProvider{dim: 7}

	dir := t.TempDir()
	dst := filepath.Join(dir, "embeddings_12.bin")

	var progressCalls []int
	progress := func(done, total int) {
		progressCalls = append(progressCalls, done)
		if total != len(corpus.Nodes) {
			t.Errorf("progress total=%d want %d", total, len(corpus.Nodes))
		}
	}

	if err := GenerateOllamaFixture(context.Background(), prov, corpus.Nodes, dst, progress); err != nil {
		t.Fatalf("GenerateOllamaFixture: %v", err)
	}

	// File exists, header reports correct dim and count.
	dim, vecs, err := ReadFixture(dst)
	if err != nil {
		t.Fatalf("ReadFixture: %v", err)
	}
	if dim != 7 {
		t.Fatalf("dim: got %d want 7", dim)
	}
	if got := len(vecs) / dim; got != len(corpus.Nodes) {
		t.Fatalf("vector count: got %d want %d", got, len(corpus.Nodes))
	}

	// Final progress call must equal total (caller wants the
	// "completed" tick to fire at the end of the run).
	if len(progressCalls) == 0 || progressCalls[len(progressCalls)-1] != len(corpus.Nodes) {
		t.Fatalf("progress: expected final call equal to total, got %v", progressCalls)
	}

	// Vector body round-trips. The generator L2-normalises every vector
	// before writing, so the stored value is the unit
	// form of the stub's raw {text[0], i, 0.}. Verify each stored vector
	// is unit-norm and matches the expected normalised first element.
	for i, n := range corpus.Nodes {
		// stubProvider sets v[0]=text[0] and v[1]=callIndex (1-based), so
		// node i was the (i+1)-th Embed call.
		raw0, raw1 := float64(n.Text[0]), float64(i+1)
		rawNorm := math.Sqrt(raw0*raw0 + raw1*raw1)
		var gotNorm float64
		for d := range dim {
			v := float64(vecs[i*dim+d])
			gotNorm += v * v
		}
		gotNorm = math.Sqrt(gotNorm)
		if math.Abs(gotNorm-1.0) > 1e-5 {
			t.Errorf("vec[%d] not unit-norm: |v|=%v", i, gotNorm)
		}
		if want := float32(raw0 / rawNorm); math.Abs(float64(vecs[i*dim]-want)) > 1e-5 {
			t.Errorf("vec[%d][0]=%v want %v (normalised)", i, vecs[i*dim], want)
		}
	}
}

func TestGenerateOllamaFixture_AtomicOnFailure(t *testing.T) {
	corpus := synthcorpus.GenerateCorpus(2, 5) // 10 nodes
	prov := &stubProvider{dim: 4, failOn: 6}   // fail mid-run

	dir := t.TempDir()
	dst := filepath.Join(dir, "embeddings_10.bin")

	err := GenerateOllamaFixture(context.Background(), prov, corpus.Nodes, dst, nil)
	if err == nil {
		t.Fatalf("expected error from induced provider failure, got nil")
	}

	// No partial fixture file: a Ctrl-C / failure mid-run must not
	// leave a half-written file at the canonical path.
	if _, statErr := os.Stat(dst); !os.IsNotExist(statErr) {
		t.Fatalf("expected no file at %s after failure, got err=%v", dst, statErr)
	}

	// No leftover temp files in the directory either.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		t.Fatalf("unexpected leftover entry after failure: %s", e.Name())
	}
}

func TestGenerateOllamaFixture_ContextCancellation(t *testing.T) {
	corpus := synthcorpus.GenerateCorpus(2, 50)
	prov := &stubProvider{dim: 4}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel

	dir := t.TempDir()
	dst := filepath.Join(dir, "embeddings_100.bin")
	err := GenerateOllamaFixture(ctx, prov, corpus.Nodes, dst, nil)
	if err == nil {
		t.Fatalf("expected cancellation error, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected ctx.Canceled, got %v", err)
	}
	if _, statErr := os.Stat(dst); !os.IsNotExist(statErr) {
		t.Fatalf("expected no file after cancellation, got err=%v", statErr)
	}
}

// TestGenerateOllamaFixture_HTTPStub exercises the real ollama.Provider
// against a local httptest server returning Ollama-shaped JSON. This
// verifies the wiring choice — generator + ports.EmbeddingProvider +
// real adapter — without depending on a live Ollama install.
func TestGenerateOllamaFixture_HTTPStub(t *testing.T) {
	// Imported here (not at top-of-file) to keep the package-level
	// imports lean for the non-stub tests above.
	const dim = 5
	const nodeCount = 6

	srv := newOllamaStub(t, dim)
	t.Cleanup(srv.Close)

	prov := newOllamaProvider(t, srv.URL)
	corpus := synthcorpus.GenerateCorpus(2, nodeCount/2)

	dst := filepath.Join(t.TempDir(), "embeddings.bin")
	if err := GenerateOllamaFixture(context.Background(), prov, corpus.Nodes, dst, nil); err != nil {
		t.Fatalf("GenerateOllamaFixture(http stub): %v", err)
	}

	d, vecs, err := ReadFixture(dst)
	if err != nil {
		t.Fatalf("ReadFixture: %v", err)
	}
	if d != dim {
		t.Fatalf("dim: got %d want %d", d, dim)
	}
	if got := len(vecs) / d; got != nodeCount {
		t.Fatalf("count: got %d want %d", got, nodeCount)
	}

	// Ensure distinct vectors came through. The stub points each request
	// along a different axis, so the full per-node vectors must differ
	// even after the generator L2-normalises them.
	seen := make(map[string]struct{})
	for i := range nodeCount {
		seen[fmt.Sprintf("%v", vecs[i*d:(i+1)*d])] = struct{}{}
	}
	if len(seen) < 2 {
		t.Fatalf("expected varying vectors across nodes, body=%v", vecs)
	}
}
