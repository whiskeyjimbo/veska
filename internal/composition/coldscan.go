// SPDX-License-Identifier: AGPL-3.0-only

package composition

import (
	"context"
	"fmt"

	"github.com/whiskeyjimbo/veska/internal/application"
	"github.com/whiskeyjimbo/veska/internal/application/checks"
	"github.com/whiskeyjimbo/veska/internal/application/staging"
	gitwatch "github.com/whiskeyjimbo/veska/internal/infrastructure/git"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/treesitter"
)

// ColdScanCore bundles the ingestion and promotion graph components shared by the
// daemon and the CLI cold-scan path: the staging area, the Ingester, the
// PromotionStore with its FTS and embedding-ref sinks, and the Promoter.
// The reparser is not part of the core because it differs between callers (the
// daemon uses a scan tracker, while the CLI does not). Callers configure
// components like finding storage and tracers via options instead of post-construction mutation.
type ColdScanCore struct {
	Staging        *staging.Area
	Gate           *staging.Gate
	Ingester       *application.Ingester
	PromotionStore *sqlite.PromotionStore
	Promoter       *application.Promoter
}

// NewColdScanCore wires the cold-scan core over the given database pools. The
// reviewEnabled option gates the review promotion lane; it is false for the CLI
// path which does not enqueue review work.
type coldScanCoreConfig struct {
	reviewEnabled  bool
	summaryEnabled bool
	vectorPruner   func(ctx context.Context, repoID, branch string, nodeIDs []string) error
}

type ColdScanCoreOption func(*coldScanCoreConfig)

// WithReviewEnabled enables the review pipeline for the cold-scan core's promotion store.
func WithReviewEnabled(enabled bool) ColdScanCoreOption {
	return func(c *coldScanCoreConfig) { c.reviewEnabled = enabled }
}

// WithSummaryEnabled enables the summary lane for the cold-scan core's promotion store.
func WithSummaryEnabled(enabled bool) ColdScanCoreOption {
	return func(c *coldScanCoreConfig) { c.summaryEnabled = enabled }
}

// WithVectorPruner wires the post-commit eviction of dropped nodes' vectors,
// typically VectorStorage.DeleteNodes. Unset on the CLI path (no live vector store).
func WithVectorPruner(fn func(ctx context.Context, repoID, branch string, nodeIDs []string) error) ColdScanCoreOption {
	return func(c *coldScanCoreConfig) { c.vectorPruner = fn }
}

func NewColdScanCore(pools *sqlite.Pools, ingesterOpts []application.IngesterOption, promoterOpts []application.PromoterOption, opts ...ColdScanCoreOption) *ColdScanCore {
	var cfg coldScanCoreConfig
	for _, opt := range opts {
		opt(&cfg)
	}
	area := staging.NewArea()
	gate := staging.NewGate(area)
	// The file walk filter is sourced from this parser's SupportedExtensions,
	// so adding a language here updates the target files.
	parser := treesitter.NewMultiParser(treesitter.NewGoParser(), treesitter.NewTSParser())
	ingester := application.NewIngester(parser, area, gate, ingesterOpts...)

	promotionStore := sqlite.NewPromotionStore(
		pools.Write,
		[]sqlite.PromotionSink{
			sqlite.NewFTSSink(),
			sqlite.NewEmbedRefSink(),
		},
		sqlite.WithReviewEnabled(cfg.reviewEnabled),
		sqlite.WithSummaryEnabled(cfg.summaryEnabled),
		sqlite.WithVectorPruner(cfg.vectorPruner),
	)
	promoter := application.NewPromoter(area, promotionStore, promoterOpts...)

	return &ColdScanCore{
		Staging:        area,
		Gate:           gate,
		Ingester:       ingester,
		PromotionStore: promotionStore,
		Promoter:       promoter,
	}
}

// NewColdScanReparser builds the cold-scan reparser closure from an
// already-wired Ingester/Promoter pair and an IgnoreLoader. The reparser is not
// folded into NewColdScanCore because the daemon needs to pass WithScanTracker,
// which is not used by the CLI.
func NewColdScanReparser(ingester *application.Ingester, promoter *application.Promoter, loader application.IgnoreLoader, opts ...application.ColdScanOption) (func(context.Context, application.RepoRecord) error, error) {
	all := append([]application.ColdScanOption{application.WithIgnoreLoader(loader)}, opts...)
	reparser, err := application.NewColdScanReparser(ingester, promoter, gitwatch.Querier{}, all...)
	if err != nil {
		return nil, fmt.Errorf("cold-scan reparser: %w", err)
	}
	return reparser, nil
}

// GitAddedLinesFunc builds the Promoter AddedLines seam from a repository root
// resolver, parsing newly-added lines via git diff. This keeps the Promoter
// package clean of infrastructure imports.
func GitAddedLinesFunc(repoRoot func(ctx context.Context, repoID string) (string, error)) application.AddedLinesFunc {
	return func(ctx context.Context, repoID, gitSHA string) (map[string][]application.Line, error) {
		root, err := repoRoot(ctx, repoID)
		if err != nil {
			return nil, err
		}
		raw, err := gitwatch.AddedLinesForCommit(ctx, root, gitSHA)
		if err != nil {
			return nil, err
		}
		out := make(map[string][]application.Line, len(raw))
		for path, lines := range raw {
			al := make([]application.Line, len(lines))
			for i, l := range lines {
				al[i] = application.Line{Number: l.Number, Text: l.Text}
			}
			out[path] = al
		}
		return out, nil
	}
}

// CheckRunnerAdapter bridges *checks.Runner to the application.CheckRunner port,
// converting application.Line to checks.Line.
type CheckRunnerAdapter struct {
	Inner *checks.Runner
}

func (a CheckRunnerAdapter) Run(ctx context.Context, in application.CheckRunInput) {
	var added map[string][]checks.Line
	if in.AddedLines != nil {
		added = make(map[string][]checks.Line, len(in.AddedLines))
		for path, lines := range in.AddedLines {
			cl := make([]checks.Line, len(lines))
			for i, l := range lines {
				cl[i] = checks.Line{Number: l.Number, Text: l.Text}
			}
			added[path] = cl
		}
	}
	a.Inner.Run(ctx, checks.Input{
		RepoID:     in.RepoID,
		Branch:     in.Branch,
		GitSHA:     in.GitSHA,
		FilePaths:  in.FilePaths,
		AddedLines: added,
	})
}
