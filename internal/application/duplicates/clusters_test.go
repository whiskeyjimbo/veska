// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package duplicates_test

import (
	"context"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/application/duplicates"
)

func TestClusters_TiersAndPrecedence(t *testing.T) {
	t.Parallel()
	store := &fakeStore{
		structuralRows: []duplicates.ClonedNode{
			// S1: both byte-identical (content h1) → EXACT tier.
			{StructuralHash: "S1", ContentHash: "h1", RepoID: "r1", NodeID: "n1", FilePath: "a.go", Kind: "function"},
			{StructuralHash: "S1", ContentHash: "h1", RepoID: "r1", NodeID: "n2", FilePath: "b.go", Kind: "function"},
			// S2: same shape, different content (h2 vs h3) → STRUCTURAL tier.
			{StructuralHash: "S2", ContentHash: "h2", RepoID: "r1", NodeID: "n3", FilePath: "c.go", Kind: "function"},
			{StructuralHash: "S2", ContentHash: "h3", RepoID: "r1", NodeID: "n4", FilePath: "d.go", Kind: "function"},
		},
		edges: []duplicates.SimilarEdge{
			// n5~n6: neither is in a hash cluster → NEAR tier.
			{Score: 0.9, Src: member("n5", "e.go", "r1"), Dst: member("n6", "f.go", "r1")},
			// n1~n7: n1 is already claimed (exact); only n7 remains → singleton → dropped.
			{Score: 0.8, Src: member("n1", "a.go", "r1"), Dst: member("n7", "g.go", "r1")},
		},
	}
	finder, _ := duplicates.NewFinder(store, store, "")

	got, err := finder.Clusters(context.Background(), duplicates.ClusterOptions{RepoID: "r1", Branch: "main"})
	if err != nil {
		t.Fatalf("Clusters: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 clusters (exact, structural, near), got %d: %+v", len(got), got)
	}
	// Ranked tightest tier first.
	wantTiers := []duplicates.Tier{duplicates.TierExact, duplicates.TierStructural, duplicates.TierNear}
	for i, w := range wantTiers {
		if got[i].Tier != w {
			t.Errorf("cluster[%d] tier = %q, want %q", i, got[i].Tier, w)
		}
	}
	// Near cluster must exclude the claimed n1 and drop the orphaned n7.
	near := got[2]
	ids := map[string]bool{}
	for _, m := range near.Members {
		ids[m.NodeID] = true
	}
	if !ids["n5"] || !ids["n6"] || ids["n7"] || ids["n1"] {
		t.Errorf("near members = %v, want exactly {n5,n6}", ids)
	}
}

func member(nodeID, file, repo string) duplicates.CloneMember {
	return duplicates.CloneMember{NodeID: nodeID, FilePath: file, RepoID: repo, Kind: "function"}
}
