package synthcorpus

import (
	"strings"
	"testing"
)

func TestGenerateSemanticCorpus_Shape(t *testing.T) {
	t.Parallel()
	c := GenerateSemanticCorpus(12)
	if c.Clusters != SemanticClusterCount {
		t.Fatalf("Clusters: want %d got %d", SemanticClusterCount, c.Clusters)
	}
	if c.NodesPerCluster != 12 {
		t.Fatalf("NodesPerCluster: want 12 got %d", c.NodesPerCluster)
	}
	if len(c.Nodes) != SemanticClusterCount*12 {
		t.Fatalf("Nodes: want %d got %d", SemanticClusterCount*12, len(c.Nodes))
	}
	if len(c.CenterQueries) != SemanticClusterCount {
		t.Fatalf("CenterQueries: want %d got %d", SemanticClusterCount, len(c.CenterQueries))
	}
	seen := make(map[string]struct{}, len(c.Nodes))
	for _, n := range c.Nodes {
		if _, dup := seen[n.NodeID]; dup {
			t.Fatalf("duplicate NodeID: %s", n.NodeID)
		}
		seen[n.NodeID] = struct{}{}
		if n.Cluster < 0 || n.Cluster >= SemanticClusterCount {
			t.Fatalf("cluster out of range: %d", n.Cluster)
		}
		if n.Text == "" {
			t.Fatalf("empty Text on %s", n.NodeID)
		}
	}
}

func TestGenerateSemanticCorpus_NodeTextDistinctWithinCluster(t *testing.T) {
	t.Parallel()
	// 100 nodes/cluster must be 100 distinct phrasings (no duplicate text
	// within a cluster) — that is what nthCombination guarantees.
	c := GenerateSemanticCorpus(100)
	byCluster := make(map[int]map[string]struct{}, c.Clusters)
	for _, n := range c.Nodes {
		if byCluster[n.Cluster] == nil {
			byCluster[n.Cluster] = make(map[string]struct{}, 100)
		}
		byCluster[n.Cluster][n.Text] = struct{}{}
	}
	for k, set := range byCluster {
		if len(set) != 100 {
			t.Fatalf("cluster %d: %d distinct node texts, want 100", k, len(set))
		}
	}
}

func TestGenerateSemanticCorpus_PhrasesDrawnFromOwnTopic(t *testing.T) {
	t.Parallel()
	// Every phrase in a node's text must come from that node's topic bag.
	// This is the property that keeps cross-cluster vocabularies disjoint.
	c := GenerateSemanticCorpus(20)
	for _, n := range c.Nodes {
		bag := semanticTopics[n.Cluster].phrases
		inBag := make(map[string]struct{}, len(bag))
		for _, p := range bag {
			inBag[p] = struct{}{}
		}
		for phrase := range strings.SplitSeq(strings.TrimSuffix(n.Text, "."), ". ") {
			if _, ok := inBag[phrase]; !ok {
				t.Fatalf("node %s (cluster %d=%s): phrase %q not in topic bag",
					n.NodeID, n.Cluster, semanticTopics[n.Cluster].name, phrase)
			}
		}
	}
}

func TestGenerateSemanticCorpus_Deterministic(t *testing.T) {
	t.Parallel()
	a := GenerateSemanticCorpus(50)
	b := GenerateSemanticCorpus(50)
	if len(a.Nodes) != len(b.Nodes) {
		t.Fatalf("lengths differ: %d vs %d", len(a.Nodes), len(b.Nodes))
	}
	for i := range a.Nodes {
		if a.Nodes[i] != b.Nodes[i] {
			t.Fatalf("non-deterministic at i=%d: %+v vs %+v", i, a.Nodes[i], b.Nodes[i])
		}
	}
}

func TestGenerateSemanticCorpus_PanicsOnOverflow(t *testing.T) {
	t.Parallel()
	t.Run("too many", func(t *testing.T) {
		defer func() {
			if recover() == nil {
				t.Fatal("expected panic when nodesPerCluster exceeds combination capacity")
			}
		}()
		_ = GenerateSemanticCorpus(semanticNodesPerClusterCap + 1)
	})
	t.Run("zero", func(t *testing.T) {
		defer func() {
			if recover() == nil {
				t.Fatal("expected panic when nodesPerCluster < 1")
			}
		}()
		_ = GenerateSemanticCorpus(0)
	})
}

func TestGenerateSemanticCorpus_TopicBagsWellFormed(t *testing.T) {
	t.Parallel()
	// Every topic must have exactly semanticPhrasesPerTopic phrases, all
	// non-empty, all distinct within the topic; topic names unique; and
	// no phrase shared across topics (the disjointness invariant).
	names := make(map[string]struct{}, len(semanticTopics))
	phraseOwner := make(map[string]string)
	for _, topic := range semanticTopics {
		if _, dup := names[topic.name]; dup {
			t.Fatalf("duplicate topic name: %s", topic.name)
		}
		names[topic.name] = struct{}{}
		if len(topic.phrases) != semanticPhrasesPerTopic {
			t.Fatalf("topic %s: %d phrases, want %d", topic.name, len(topic.phrases), semanticPhrasesPerTopic)
		}
		local := make(map[string]struct{}, len(topic.phrases))
		for _, p := range topic.phrases {
			if p == "" {
				t.Fatalf("topic %s: empty phrase", topic.name)
			}
			if _, dup := local[p]; dup {
				t.Fatalf("topic %s: duplicate phrase %q", topic.name, p)
			}
			local[p] = struct{}{}
			if owner, ok := phraseOwner[p]; ok {
				t.Fatalf("phrase %q shared across topics %s and %s — vocabularies must be disjoint",
					p, owner, topic.name)
			}
			phraseOwner[p] = topic.name
		}
	}
}

func TestNthCombination(t *testing.T) {
	t.Parallel()
	// Enumerate all C(6,3)=20 combinations: they must be distinct, sorted,
	// and in lexicographic order.
	const total, k = 6, 3
	count := binomial(total, k)
	if count != 20 {
		t.Fatalf("binomial(6,3): want 20 got %d", count)
	}
	seen := make(map[string]struct{}, count)
	var prev []int
	for n := range count {
		c := nthCombination(n, total, k)
		if len(c) != k {
			t.Fatalf("n=%d: len %d want %d", n, len(c), k)
		}
		for i := 1; i < len(c); i++ {
			if c[i] <= c[i-1] {
				t.Fatalf("n=%d: not strictly ascending: %v", n, c)
			}
		}
		key := joinInts(c)
		if _, dup := seen[key]; dup {
			t.Fatalf("n=%d: duplicate combination %v", n, c)
		}
		seen[key] = struct{}{}
		if prev != nil && joinInts(c) <= joinInts(prev) {
			t.Fatalf("n=%d: not lexicographic: %v after %v", n, c, prev)
		}
		prev = c
	}
	// Index wraps modulo count.
	if joinInts(nthCombination(count, total, k)) != joinInts(nthCombination(0, total, k)) {
		t.Fatal("nthCombination did not wrap modulo C(n,k)")
	}
}

func joinInts(xs []int) string {
	var b strings.Builder
	for _, x := range xs {
		b.WriteByte(byte('0' + x))
	}
	return b.String()
}
