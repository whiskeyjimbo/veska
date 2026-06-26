// SPDX-License-Identifier: AGPL-3.0-only

package mcp

import (
	"context"
	"encoding/json"
	"fmt"

	application "github.com/whiskeyjimbo/veska/internal/application"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// dupSeed selects which duplicate-finding strategy eng_find_duplicates runs.
// The four former dedup tools (search_similar, find_related, find_clones,
// find_clusters) are merged into one tool selected by this seed so the agent
// surface carries a single "avoid duplication" entry point.
type dupSeed string

const (
	// dupSeedClusters is the whole-repo tiered view (exact+structural+near),
	// the default - "find all the duplication in this repo".
	dupSeedClusters dupSeed = "clusters"
	// dupSeedClones is the whole-repo exact/near clone view (mode-selected).
	dupSeedClones dupSeed = "clones"
	// dupSeedSimilar is the seeded "what looks like THIS one symbol" view.
	dupSeedSimilar dupSeed = "similar"
	// dupSeedRelated is the seeded "what looks like the code at this
	// (file_path, line)" view.
	dupSeedRelated dupSeed = "related"
)

// DescFindDuplicates documents the merged dedup tool. It folds in the four
// former tools' guidance and the grep-first routing rule from the A/B bench
// (dedup loses to grep when an obvious literal exists).
const DescFindDuplicates = "Find duplicate / near-duplicate code, for the 'avoid duplication' goal. Best when there is NO obvious grep string to match on; if a literal copy-paste shares a searchable token, grep is cheaper. The 'seed' param selects the strategy: seed=clusters (default) returns whole-repo tiered clusters (exact byte-identical, structural Type-2, and vector-near), ranked tightest first, each member with repo_id/file/line for a dedupe task - scope=all clusters across every registered repo; seed=clones returns whole-repo groups for a single mode (mode=exact byte-identical via content_hash, or mode=near thresholded SIMILAR_TO clusters); seed=similar returns vector-nearest neighbors of ONE existing symbol (pass node_id or symbol) - 'what else looks like this?'; seed=related does the same seeded by a (file_path, line) cursor. NOTE: structural/near tiers need structural_hash + scored SIMILAR_TO edges from a promotion/reindex on a current build."

// duplicatesSeedSelector peeks at the seed param without consuming the rest of
// the payload (each sub-handler binds its own params from raw).
type duplicatesSeedSelector struct {
	Seed string `json:"seed"`
}

// RegisterDuplicatesTool registers the merged eng_find_duplicates tool. It
// builds the four former dedup handlers from their existing constructors so each
// seed mode preserves its exact behavior and response shape; the tool returns
// the active mode's response unchanged (the handler return type is any).
func RegisterDuplicatesTool(
	r *Registry,
	finder CloneFinder,
	lookup SimilarLookup,
	vectors ports.VectorStorage,
	nodes ports.NodeLookup,
	repos application.RepoLister,
	graph ports.GraphReader,
) {
	clusters := makeFindClustersHandler(finder, repos)
	clones := makeFindClonesHandler(finder, repos)
	similar := makeSearchSimilarHandler(lookup, vectors, nodes, repos, graph)
	related := makeFindRelatedHandler(lookup, vectors, nodes, repos)
	r.MustRegister(ToolSpec{
		Name:        "eng_find_duplicates",
		Description: DescFindDuplicates,
		// search_similar carried Tier1; the merged tool inherits it.
		Tier:            Tier1,
		IncludesStaging: false,
		InputSchema:     findDuplicatesInputSchema,
		Handler:         makeDuplicatesHandler(clusters, clones, similar, related),
	})
}

// makeDuplicatesHandler routes eng_find_duplicates to the seed-selected
// sub-handler. Empty defaults to clusters (the whole-repo "find all
// duplication" view that matches the tool name's intent).
func makeDuplicatesHandler(clusters, clones, similar, related ToolHandler) ToolHandler {
	return func(ctx context.Context, actor domain.Actor, raw json.RawMessage) (any, *RPCError) {
		var sel duplicatesSeedSelector
		if rpcErr := bindParams(raw, &sel); rpcErr != nil {
			return nil, rpcErr
		}
		switch dupSeed(sel.Seed) {
		case "", dupSeedClusters:
			return clusters(ctx, actor, raw)
		case dupSeedClones:
			return clones(ctx, actor, raw)
		case dupSeedSimilar:
			return similar(ctx, actor, raw)
		case dupSeedRelated:
			return related(ctx, actor, raw)
		default:
			return nil, &RPCError{Code: CodeInvalidParams, Message: fmt.Sprintf("unknown seed %q (want %q, %q, %q or %q)", sel.Seed, dupSeedClusters, dupSeedClones, dupSeedSimilar, dupSeedRelated)}
		}
	}
}
