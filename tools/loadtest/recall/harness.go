package recall

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/whiskeyjimbo/veska/tools/loadtest/synthcorpus"
)

// Re-exports from synthcorpus.
// The synthetic corpus generator and fake embedder were extracted into
// tools/loadtest/synthcorpus so both the recall and the autolink eval
// harnesses can share them. These aliases preserve the in-package
// surface the eval test was written against (recall.GenerateCorpus,
// recall.FakeEmbedder, etc.) without breaking the call sites.

const FakeEmbeddingDim = synthcorpus.FakeEmbeddingDim

type (
	SyntheticNode = synthcorpus.SyntheticNode
	Corpus        = synthcorpus.Corpus
	FakeEmbedder  = synthcorpus.FakeEmbedder
)

// GenerateCorpus delegates to synthcorpus.GenerateCorpus.
func GenerateCorpus(clusters, nodesPerCluster int) Corpus {
	return synthcorpus.GenerateCorpus(clusters, nodesPerCluster)
}

// GenerateSemanticCorpus delegates to synthcorpus.GenerateSemanticCorpus.
// The semantic corpus is required for the gate-3 auto-link FP measurement
// against real embedding models; see synthcorpus/semantic.go. Its cluster
// count is fixed at synthcorpus.SemanticClusterCount.
func GenerateSemanticCorpus(nodesPerCluster int) Corpus {
	return synthcorpus.GenerateSemanticCorpus(nodesPerCluster)
}

// SemanticClusterCount re-exports synthcorpus.SemanticClusterCount.
var SemanticClusterCount = synthcorpus.SemanticClusterCount

// Fixture I/O

// FixtureHeader is the on-disk preamble for a cached embedding fixture.
// The format is intentionally trivial - little-endian uint32 dim, uint32
// count, then count*dim float32s - so a regenerated fixture is bit-stable
// across runs of the same harness binary.
type FixtureHeader struct {
	Dim   uint32
	Count uint32
}

// WriteFixture serialises vectors to path. vectors must be a flat slice
// of count*dim float32s.
func WriteFixture(path string, dim int, vectors []float32) error {
	if dim <= 0 {
		return errors.New("recall: WriteFixture dim must be > 0")
	}
	if len(vectors)%dim != 0 {
		return fmt.Errorf("recall: WriteFixture: vectors len %d not divisible by dim %d", len(vectors), dim)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("recall: WriteFixture mkdir: %w", err)
	}
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("recall: WriteFixture create: %w", err)
	}
	defer f.Close()

	hdr := FixtureHeader{Dim: uint32(dim), Count: uint32(len(vectors) / dim)}
	if err := binary.Write(f, binary.LittleEndian, hdr); err != nil {
		return fmt.Errorf("recall: WriteFixture header: %w", err)
	}
	if err := binary.Write(f, binary.LittleEndian, vectors); err != nil {
		return fmt.Errorf("recall: WriteFixture body: %w", err)
	}
	return nil
}

// ReadFixture loads a fixture previously written by WriteFixture and
// returns (dim, flat vectors).
func ReadFixture(path string) (int, []float32, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, nil, fmt.Errorf("recall: ReadFixture open: %w", err)
	}
	defer f.Close()

	var hdr FixtureHeader
	if err := binary.Read(f, binary.LittleEndian, &hdr); err != nil {
		return 0, nil, fmt.Errorf("recall: ReadFixture header: %w", err)
	}
	if hdr.Dim == 0 || hdr.Count == 0 {
		return 0, nil, fmt.Errorf("recall: ReadFixture: corrupt header (dim=%d count=%d)", hdr.Dim, hdr.Count)
	}
	out := make([]float32, int(hdr.Dim)*int(hdr.Count))
	if err := binary.Read(f, binary.LittleEndian, out); err != nil && !errors.Is(err, io.EOF) {
		return 0, nil, fmt.Errorf("recall: ReadFixture body: %w", err)
	}
	return int(hdr.Dim), out, nil
}

// FixturePath returns the conventional path for the cached embedding
// fixture at the given population size, relative to dir.
func FixturePath(dir string, population int) string {
	return filepath.Join(dir, fmt.Sprintf("embeddings_%d.bin", population))
}

// Result envelope

// Result is the JSON envelope written by the eval harness. Field names
// match the DoD exactly so downstream M3-close tooling can read it
// without translation.
type Result struct {
	Population      int       `json:"population"`
	Clusters        int       `json:"clusters"`
	NodesPerCluster int       `json:"nodes_per_cluster"`
	Queries         int       `json:"queries"`
	MeanRecall      float64   `json:"mean_recall"`
	P95LatencyMs    float64   `json:"p95_latency_ms"`
	Embedder        string    `json:"embedder"`
	Backend         string    `json:"backend"`
	Timestamp       time.Time `json:"timestamp"`
}

// WriteJSON writes r to path as pretty-printed JSON.
func WriteJSON(path string, r Result) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("recall: WriteJSON mkdir: %w", err)
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("recall: WriteJSON marshal: %w", err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		return fmt.Errorf("recall: WriteJSON write: %w", err)
	}
	return nil
}

// SortedDurations returns a copy of samples sorted ascending. Useful
// alongside P95Latency when callers want to log additional percentiles.
func SortedDurations(samples []time.Duration) []time.Duration {
	out := make([]time.Duration, len(samples))
	copy(out, samples)
	slices.Sort(out)
	return out
}
