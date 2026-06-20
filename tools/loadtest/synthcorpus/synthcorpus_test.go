// SPDX-License-Identifier: AGPL-3.0-only

package synthcorpus

import (
	"context"
	"math"
	"testing"
)

func TestGenerateCorpus_Shape(t *testing.T) {
	t.Parallel()
	c := GenerateCorpus(5, 4)
	if c.Clusters != 5 || c.NodesPerCluster != 4 {
		t.Fatalf("shape: got clusters=%d nodes_per_cluster=%d", c.Clusters, c.NodesPerCluster)
	}
	if len(c.Nodes) != 20 {
		t.Fatalf("Nodes: want 20, got %d", len(c.Nodes))
	}
	if len(c.CenterQueries) != 5 {
		t.Fatalf("CenterQueries: want 5, got %d", len(c.CenterQueries))
	}
	// Each node must have a non-empty NodeID and a Cluster within range.
	for _, n := range c.Nodes {
		if n.NodeID == "" {
			t.Fatalf("empty NodeID")
		}
		if n.Cluster < 0 || n.Cluster >= 5 {
			t.Fatalf("cluster out of range: %d", n.Cluster)
		}
	}
}

func TestTruthByCluster(t *testing.T) {
	t.Parallel()
	c := GenerateCorpus(3, 4)
	truth := c.TruthByCluster()
	if len(truth) != 3 {
		t.Fatalf("len truth: %d", len(truth))
	}
	for k := range 3 {
		if len(truth[k]) != 4 {
			t.Fatalf("truth[%d] size: %d", k, len(truth[k]))
		}
	}
}

func TestClusterOf(t *testing.T) {
	t.Parallel()
	c := GenerateCorpus(3, 2)
	lookup := c.ClusterOf()
	if len(lookup) != 6 {
		t.Fatalf("len lookup: %d", len(lookup))
	}
	for _, n := range c.Nodes {
		if got := lookup[n.NodeID]; got != n.Cluster {
			t.Fatalf("ClusterOf[%s]: want %d got %d", n.NodeID, n.Cluster, got)
		}
	}
}

func TestParseClusterID(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		k    int
		okay bool
	}{
		{"cluster_0_member_0", 0, true},
		{"cluster_12_centroid", 12, true},
		{"cluster_abc_x", 0, false},
		{"prefix_cluster_1_x", 0, false},
		{"cluster_", 0, false},
		{"", 0, false},
	}
	for _, tc := range cases {
		k, ok := ParseClusterID(tc.in)
		if ok != tc.okay || (ok && k != tc.k) {
			t.Fatalf("ParseClusterID(%q): want (%d,%v) got (%d,%v)", tc.in, tc.k, tc.okay, k, ok)
		}
	}
}

func TestFakeEmbed_Deterministic_AndNormalized(t *testing.T) {
	t.Parallel()
	v1 := FakeEmbed("cluster_3_member_7")
	v2 := FakeEmbed("cluster_3_member_7")
	if len(v1) != FakeEmbeddingDim {
		t.Fatalf("dim: got %d want %d", len(v1), FakeEmbeddingDim)
	}
	for i := range v1 {
		if v1[i] != v2[i] {
			t.Fatalf("non-deterministic at i=%d", i)
		}
	}
	var sq float64
	for _, x := range v1 {
		sq += float64(x) * float64(x)
	}
	if math.Abs(sq-1.0) > 1e-5 {
		t.Fatalf("not normalized: |v|^2 = %v", sq)
	}
}

func TestFakeEmbed_ClusterSpike(t *testing.T) {
	t.Parallel()
	// The member and centroid for the same cluster should share a
	// dominant axis; cross-cluster vectors should have lower inner
	// product than intra-cluster.
	a := FakeEmbed("cluster_2_member_0")
	b := FakeEmbed("cluster_2_member_1")
	c := FakeEmbed("cluster_7_member_0")
	dot := func(x, y []float32) float64 {
		var s float64
		for i := range x {
			s += float64(x[i]) * float64(y[i])
		}
		return s
	}
	intra := dot(a, b)
	inter := dot(a, c)
	if intra <= inter {
		t.Fatalf("expected intra (%v) > inter (%v) cluster similarity", intra, inter)
	}
}

func TestFakeEmbedder_PortShape(t *testing.T) {
	t.Parallel()
	var e FakeEmbedder
	if e.ModelID() == "" {
		t.Fatalf("ModelID empty")
	}
	v, err := e.Embed(context.Background(), "cluster_0_member_0")
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(v) != FakeEmbeddingDim {
		t.Fatalf("Embed dim: %d", len(v))
	}
}
