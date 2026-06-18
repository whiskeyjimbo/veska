// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package duplicates

import (
	"context"
	"fmt"
	"sort"
)

// Tier classifies a cluster by its similarity type: exact (byte-identical),
// structural (Type-2 shape matches), or near (vector-similar).
type Tier string

const (
	TierExact      Tier = "exact"
	TierStructural Tier = "structural"
	TierNear       Tier = "near"
)

var tierRank = map[Tier]int{TierExact: 0, TierStructural: 1, TierNear: 2}

// Cluster groups similar nodes at a single Tier. Score represents the weakest
// edge similarity score (0 if not TierNear).
type Cluster struct {
	Tier      Tier
	Members   []CloneMember
	Size      int
	Score     float32
	CrossRepo bool
}

// ClusterOptions filters the cluster results. If AllRepos is true, the near tier
// is skipped because cross-repo similar-to edges are not persisted.
type ClusterOptions struct {
	RepoID     string
	Branch     string
	AllRepos   bool
	PathPrefix string
	Tiers      []Tier
	MinScore   float32
}

// Clusters aggregates and ranks similar-code groups. Groups are ranked by tier
// priority (exact > structural > near), descending group size, and stably by the
// first member's ID.
func (f *Finder) Clusters(ctx context.Context, opts ClusterOptions) ([]Cluster, error) {
	want := tierFilter(opts.Tiers)
	claimed := make(map[string]bool) // node_ids already in a hash cluster
	out := make([]Cluster, 0)

	q := CloneQuery{Branch: opts.Branch, PathPrefix: opts.PathPrefix}
	if !opts.AllRepos {
		q.RepoID = opts.RepoID // empty RepoID => all repos in the store layer
	}

	// Structural grouping is the superset of exact; sub-tier each group by
	// whether all its members are byte-identical.
	structRows, err := f.clones.StructuralNodes(ctx, q, ExcludedKinds)
	if err != nil {
		return nil, fmt.Errorf("duplicates.Clusters: %w", err)
	}
	structGroups := groupByHash(structRows, func(r ClonedNode) string { return r.StructuralHash })
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

	// Near clusters: the looser tier (intra-repo only - skipped in AllRepos
	// mode, see ClusterOptions). Drop members already structurally grouped so a
	// node appears at most once, at its tightest tier.
	if want[TierNear] && !opts.AllRepos {
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

func spansRepos(members []CloneMember) bool {
	for _, m := range members[1:] {
		if m.RepoID != members[0].RepoID {
			return true
		}
	}
	return false
}
