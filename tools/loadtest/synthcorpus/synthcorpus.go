// Package synthcorpus generates deterministic synthetic node corpora and
// a deterministic hash-to-vector embedder used by the loadtest harnesses
// (recall@k in tools/loadtest/recall and auto-link FP-rate in
// tools/loadtest/autolink).
//
// Ground truth is structural: two nodes are "related" iff they live in
// the same cluster. The cluster id is encoded both in the node's
// metadata (Cluster int) and in its synthetic Text so the FakeEmbedder
// can produce a vector with a strong spike on the cluster axis without
// needing access to the labelled metadata.
//
// Both harnesses depend on this package; the recall harness layers a
// "center query" per cluster on top, while the autolink harness uses
// existing node embeddings as the query.
package synthcorpus

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"math"
)

// FakeEmbeddingDim is the dimensionality of the deterministic fake
// embedder. We pick 64 — large enough that the cluster centroid
// projection is well-conditioned, small enough that 250k vectors fit
// comfortably in memory for the sqlite-vec linear-scan backend.
const FakeEmbeddingDim = 64

// SyntheticNode is one entry in the generated corpus. NodeID is the
// stable identifier used for ground-truth comparison; Cluster is the
// integer cluster id (the "ground truth label") and Text is the
// deterministic source text the embedder turns into a vector.
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
	// CenterQueries[i] is the query text designating cluster i. The
	// FakeEmbedder maps each center query to a vector that aligns with
	// cluster i's members.
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

// ClusterOf builds a NodeID -> cluster index lookup over the corpus.
// Used by the autolink harness to classify candidate pairs as
// true-positive (same cluster) or false-positive (different cluster).
func (c Corpus) ClusterOf() map[string]int {
	out := make(map[string]int, len(c.Nodes))
	for _, n := range c.Nodes {
		out[n.NodeID] = n.Cluster
	}
	return out
}

// FakeEmbedder is a deterministic hash-to-vector embedder used by the
// quick-mode harnesses. It satisfies ports.EmbeddingProvider structurally
// (Embed + ModelID); callers that need the interface adapter can pass
// FakeEmbedder{} directly.
type FakeEmbedder struct{}

// Embed implements ports.EmbeddingProvider.
func (FakeEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	return FakeEmbed(text), nil
}

// ModelID implements ports.EmbeddingProvider.
func (FakeEmbedder) ModelID() string { return "fake-hash-v1" }

// FakeEmbed builds a deterministic FakeEmbeddingDim vector from text.
//
// The construction has two parts so cluster-center queries align with
// their own members:
//
//  1. A cluster bias: ParseClusterID extracts the integer K from
//     "cluster_K_*" prefixes; the vector then has a strong positive
//     spike on axis (K mod FakeEmbeddingDim). Both centroid and member
//     texts share this prefix, so they share the bias direction.
//  2. A per-text jitter: the SHA-256 of the full text contributes
//     small-magnitude noise across all axes so different members of
//     the same cluster aren't literal duplicates.
//
// The whole vector is L2-normalised so cosine and L2-squared scoring
// behave consistently.
func FakeEmbed(text string) []float32 {
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
	if k, ok := ParseClusterID(text); ok {
		axis := k % FakeEmbeddingDim
		// Strong positive spike on the cluster axis (relative to the
		// 0.05-scale jitter, this dominates ranking).
		vec[axis] += 1.0
	}
	normalise(vec)
	return vec
}

// ParseClusterID returns the cluster index K when text starts with
// "cluster_K_". Returns ok=false otherwise.
func ParseClusterID(text string) (int, bool) {
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
