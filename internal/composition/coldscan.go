// Package composition holds composition-root helpers shared by more than one
// entry point — currently the daemon (internal/cli/daemon) and the CLI
// cold-scan path (cmd/veska reindex/search). It exists so the ingestion +
// promotion wiring is defined once instead of duplicated across cmd/veska and
// the daemon and kept in lock-step by hand-written comments (solov2-u4mv).
//
// It is a composition root, so it may import the infrastructure adapters; the
// hexagonal rule that domain/ports must not import infra still holds and is
// enforced by `make layercheck`.
package composition

import (
	"context"

	"github.com/whiskeyjimbo/veska/internal/application"
	"github.com/whiskeyjimbo/veska/internal/application/checks"
	gitwatch "github.com/whiskeyjimbo/veska/internal/infrastructure/git"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/treesitter"
)

// ColdScanCore bundles the ingestion + promotion graph shared by the daemon and
// the CLI cold-scan (reindex) path: the staging area, the Ingester, the
// PromotionStore with its FTS + embedding-ref sinks, and the Promoter.
//
// Check registration, the reparser, and finding storage are intentionally NOT
// part of the core — they legitimately differ between callers (the daemon
// registers dead-code + contract-drift checks the CLI omits, passes a scan
// tracker to the reparser, and shares a metrics registry). The core is exactly
// the part that was previously copied verbatim between cmd/veska/reindex.go and
// internal/cli/daemon/wire.go.
type ColdScanCore struct {
	Staging        *application.StagingArea
	Gate           *application.IngestionGate
	Ingester       *application.Ingester
	PromotionStore *sqlite.PromotionStore
	Promoter       *application.Promoter
}

// NewColdScanCore wires the cold-scan core over the given pools. reviewEnabled
// gates the optional WorkKindReview promotion lane (sqlite.WithReviewEnabled);
// pass false for the CLI path, which never enqueues review work.
func NewColdScanCore(pools *sqlite.Pools, reviewEnabled bool) *ColdScanCore {
	staging := application.NewStagingArea()
	gate := application.NewIngestionGate(staging)
	parser := treesitter.NewGoParser()
	ingester := application.NewIngester(parser, staging, gate)

	promotionStore := sqlite.NewPromotionStore(
		pools.Write,
		[]sqlite.PromotionSink{
			sqlite.NewFTSSink(),
			sqlite.NewEmbedRefSink(),
		},
		sqlite.WithReviewEnabled(reviewEnabled),
	)
	promoter := application.NewPromoter(staging, promotionStore)

	return &ColdScanCore{
		Staging:        staging,
		Gate:           gate,
		Ingester:       ingester,
		PromotionStore: promotionStore,
		Promoter:       promoter,
	}
}

// GitAddedLinesFunc builds the Promoter AddedLines seam from a repo-root
// resolver: it resolves the repo's working-tree root, then parses the promoted
// commit's newly-added lines via git diff. Shared so the daemon and the CLI
// cold-scan path resolve diffs identically (the closure was previously copied
// into both). Keeps the Promoter free of an infrastructure import.
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

// CheckRunnerAdapter bridges *checks.Runner to the application.CheckRunner port
// the Promoter consumes, converting application.Line to checks.Line. Both the
// daemon and the CLI cold-scan path wrap their runner in this — previously two
// byte-identical copies (daemon.checkRunnerAdapter, reindex.coldScanCheckRunner).
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
