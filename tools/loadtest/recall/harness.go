package recall

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"time"
)

// FakeEmbeddingDim is the dimensionality of the deterministic fake
// embedder. We pick 64 — large enough that the cluster centroid
// projection is well-conditioned, small enough that 250k vectors fit
// comfortably in memory for the sqlite-vec linear-scan backend.
const FakeEmbeddingDim = 64

// SyntheticNode is one entry in the generated corpus. NodeID is the
// stable identifier used both for ground-truth comparison and for
// hydration via NodeLookup; Cluster is the integer cluster id (the
// "ground truth label") and Text is the deterministic source text the
// embedder turns into a vector.
type SyntheticNode struct {
	NodeID     string
	Cluster    int
	Text       string
	SymbolPath string
	FilePath   string
	Kind       string
}

// Corpus is a generated synthetic dataset: a fixed number of clusters,
// each with NodesPerCluster members, plus one center query per cluster.
type Corpus struct {
	Clusters        int
	NodesPerCluster int
	Nodes           []SyntheticNode
	// CenterQueries[i] is the query text designating cluster i.
	CenterQueries []string
}

// GenerateCorpus builds a deterministic synthetic corpus. Total node
// count is clusters * nodesPerCluster.
func GenerateCorpus(clusters, nodesPerCluster int) Corpus {
	c := Corpus{
		Clusters:        clusters,
		NodesPerCluster: nodesPerCluster,
		Nodes:           make([]SyntheticNode, 0, clusters*nodesPerCluster),
		CenterQueries:   make([]string, clusters),
	}
	for k := range clusters {
		c.CenterQueries[k] = fmt.Sprintf("cluster_%d_centroid", k)
		for j := range nodesPerCluster {
			id := fmt.Sprintf("c%d_n%d", k, j)
			c.Nodes = append(c.Nodes, SyntheticNode{
				NodeID:     id,
				Cluster:    k,
				Text:       fmt.Sprintf("cluster_%d_member_%d", k, j),
				SymbolPath: fmt.Sprintf("synth.cluster%d.member%d", k, j),
				FilePath:   fmt.Sprintf("synth/cluster_%d.go", k),
				Kind:       "function",
			})
		}
	}
	return c
}

// TruthByCluster returns the ground-truth NodeID set for each cluster,
// indexed by cluster id. truth[k] is the set of NodeIDs that count as
// correct hits for CenterQueries[k].
func (c Corpus) TruthByCluster() []map[string]struct{} {
	out := make([]map[string]struct{}, c.Clusters)
	for k := range out {
		out[k] = make(map[string]struct{}, c.NodesPerCluster)
	}
	for _, n := range c.Nodes {
		out[n.Cluster][n.NodeID] = struct{}{}
	}
	return out
}

// FakeEmbedder is a deterministic hash-to-vector embedder used by the
// harness's quick mode. It implements ports.EmbeddingProvider without
// importing the ports package directly (the harness consumes the port
// indirectly through search.Service, but the embedder shape matches
// the interface). The vector is the L2-normalised projection of a
// SHA-256 stretched into FakeEmbeddingDim float32s, with cluster-aware
// bias: when text matches the cluster_K_* / cluster_K_centroid
// convention, the cluster axis dominates so cluster-center queries
// rank their own cluster members highly.
type FakeEmbedder struct{}

// Embed implements ports.EmbeddingProvider.
func (FakeEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	return fakeEmbed(text), nil
}

// ModelID implements ports.EmbeddingProvider.
func (FakeEmbedder) ModelID() string { return "fake-hash-v1" }

// fakeEmbed builds a deterministic FakeEmbeddingDim vector from text.
//
// The construction has two parts so cluster-center queries align with
// their own members:
//
//  1. A cluster bias: parseCluster extracts the integer K from
//     "cluster_K_*" prefixes; the vector then has a strong positive
//     spike on axis (K mod FakeEmbeddingDim). Both centroid and member
//     texts share this prefix, so they share the bias direction.
//  2. A per-text jitter: the SHA-256 of the full text contributes
//     small-magnitude noise across all axes so different members of
//     the same cluster aren't literal duplicates.
//
// The whole vector is L2-normalised so cosine and L2-squared scoring
// behave consistently.
func fakeEmbed(text string) []float32 {
	vec := make([]float32, FakeEmbeddingDim)
	h := sha256.Sum256([]byte(text))
	for i := range FakeEmbeddingDim {
		// Stretch 32 bytes into FakeEmbeddingDim floats by hashing
		// successive 8-byte windows reinterpreted as uint64.
		u := binary.LittleEndian.Uint64(h[(i*8)%32 : (i*8)%32+8])
		// Map to [-1, 1] then scale down so the cluster spike dominates.
		v := (float32(u%2000)/1000.0 - 1.0) * 0.05
		vec[i] = v
	}
	if k, ok := parseClusterID(text); ok {
		axis := k % FakeEmbeddingDim
		// Strong positive spike on the cluster axis (relative to the
		// 0.05-scale jitter, this dominates ranking).
		vec[axis] += 1.0
	}
	normalise(vec)
	return vec
}

// parseClusterID returns the cluster index K when text starts with
// "cluster_K_". Returns ok=false otherwise.
func parseClusterID(text string) (int, bool) {
	const prefix = "cluster_"
	if len(text) < len(prefix)+2 || text[:len(prefix)] != prefix {
		return 0, false
	}
	// Scan digits after "cluster_" until the next underscore.
	i := len(prefix)
	start := i
	for i < len(text) && text[i] >= '0' && text[i] <= '9' {
		i++
	}
	if i == start || i >= len(text) || text[i] != '_' {
		return 0, false
	}
	k := 0
	for j := start; j < i; j++ {
		k = k*10 + int(text[j]-'0')
	}
	return k, true
}

func normalise(v []float32) {
	var sq float64
	for _, x := range v {
		sq += float64(x) * float64(x)
	}
	n := math.Sqrt(sq)
	if n == 0 {
		return
	}
	for i := range v {
		v[i] = float32(float64(v[i]) / n)
	}
}

// --- Fixture I/O -----------------------------------------------------------

// FixtureHeader is the on-disk preamble for a cached embedding fixture.
// The format is intentionally trivial — little-endian uint32 dim, uint32
// count, then count*dim float32s — so a regenerated fixture is bit-stable
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

// --- Result envelope -------------------------------------------------------

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
