package duplicates

import (
	"context"
	"fmt"
	"sort"
)

// Tier ranks a cluster by the precision of its similarity signal, tightest
// first. The unified Clusters view labels every group with one.
type Tier string

const (
	// TierExact: every member is byte-identical (shared content_hash).
	TierExact Tier = "exact"
	// TierStructural: same shape after a consistent rename (shared
	// structural_hash) but NOT all byte-identical — Type-2 clones.
	TierStructural Tier = "structural"
	// TierNear: vector-similar (SIMILAR_TO above threshold), looser than the
	// hash tiers; only members not already in a hash cluster appear here.
	TierNear Tier = "near"
)

var tierRank = map[Tier]int{TierExact: 0, TierStructural: 1, TierNear: 2}

// Cluster is one group of >=2 similar nodes at a single Tier. Score is the
// weakest edge score in the component for the near tier and 0 otherwise.
// CrossRepo is true when the members span more than one repo.
type Cluster struct {
	Tier      Tier
	Members   []CloneMember
	Size      int
	Score     float32
	CrossRepo bool
}

// ClusterOptions configures Clusters. Empty Tiers means all tiers. MinScore <= 0
// uses the calibrated near-dup default for the elected embedder.
type ClusterOptions struct {
	RepoID   string
	Branch   string
	Tiers    []Tier
	MinScore float32
}

// Clusters returns the unified, tier-labeled similar-code view for one repo: the
// structural grouping (a superset of exact) sub-tiered into exact vs structural,
// plus near clusters for any node not already in a hash cluster (precedence
// exact > structural > near). Ranked tightest tier first, then by descending
// size, then stably by the first member's node id.
func (f *Finder) Clusters(ctx context.Context, opts ClusterOptions) ([]Cluster, error) {
	want := tierFilter(opts.Tiers)
	claimed := make(map[string]bool) // node_ids already in a hash cluster
	out := make([]Cluster, 0)

	// Structural grouping is the superset of exact; sub-tier each group by
	// whether all its members are byte-identical.
	structGroups, err := f.StructuralClones(ctx, opts.RepoID, opts.Branch)
	if err != nil {
		return nil, fmt.Errorf("duplicates.Clusters: %w", err)
	}
	for _, g := range structGroups {
		for _, m := range g.Members {
			claimed[m.NodeID] = true
		}
		tier := TierStructural
		if uniformContentHash(g.Members) {
			tier = TierExact
		}
		if want[tier] {
			out = append(out, Cluster{
				Tier: tier, Members: g.Members, Size: g.Size,
				CrossRepo: spansRepos(g.Members),
			})
		}
	}

	// Near clusters: the looser tier. Drop members already structurally grouped
	// so a node appears at most once, at its tightest tier.
	if want[TierNear] {
		nears, err := f.NearDuplicates(ctx, opts.RepoID, opts.Branch, opts.MinScore)
		if err != nil {
			return nil, fmt.Errorf("duplicates.Clusters: %w", err)
		}
		for _, nc := range nears {
			kept := make([]CloneMember, 0, len(nc.Members))
			for _, m := range nc.Members {
				if !claimed[m.NodeID] {
					kept = append(kept, m)
				}
			}
			if len(kept) < 2 {
				continue
			}
			out = append(out, Cluster{
				Tier: TierNear, Members: kept, Size: len(kept),
				Score: nc.MinScore, CrossRepo: spansRepos(kept),
			})
		}
	}

	sort.SliceStable(out, func(i, j int) bool {
		if tierRank[out[i].Tier] != tierRank[out[j].Tier] {
			return tierRank[out[i].Tier] < tierRank[out[j].Tier]
		}
		if out[i].Size != out[j].Size {
			return out[i].Size > out[j].Size
		}
		return out[i].Members[0].NodeID < out[j].Members[0].NodeID
	})
	return out, nil
}

// tierFilter turns the requested tier list into a membership set; an empty list
// means "all tiers".
func tierFilter(tiers []Tier) map[Tier]bool {
	if len(tiers) == 0 {
		return map[Tier]bool{TierExact: true, TierStructural: true, TierNear: true}
	}
	m := make(map[Tier]bool, len(tiers))
	for _, t := range tiers {
		m[t] = true
	}
	return m
}

// uniformContentHash reports whether every member shares one content_hash (a
// fully byte-identical group — the exact tier).
func uniformContentHash(members []CloneMember) bool {
	if len(members) == 0 {
		return false
	}
	first := members[0].ContentHash
	for _, m := range members[1:] {
		if m.ContentHash != first {
			return false
		}
	}
	return true
}

// spansRepos reports whether the members come from more than one repo.
func spansRepos(members []CloneMember) bool {
	for _, m := range members[1:] {
		if m.RepoID != members[0].RepoID {
			return true
		}
	}
	return false
}
