package composition

import (
	"context"

	"github.com/whiskeyjimbo/veska/internal/application"
	"github.com/whiskeyjimbo/veska/internal/application/checks"
	"github.com/whiskeyjimbo/veska/internal/application/staging"
	gitwatch "github.com/whiskeyjimbo/veska/internal/infrastructure/git"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/treesitter"
)

// ColdScanCore bundles the ingestion + promotion graph shared by the daemon and
// the CLI cold-scan (reindex) path: the staging area, the Ingester, the
// PromotionStore with its FTS + embedding-ref sinks, and the Promoter.
//
// The reparser is intentionally NOT part of the core — it legitimately differs
// between callers (the daemon passes a scan tracker). The Ingester's finding
// storage and the Promoter's check/added-lines/tracer seams also differ per
// caller (the daemon registers dead-code + contract-drift checks the CLI omits,
// and only the daemon emits parse findings), so they are supplied by the caller
// as construction options via ingesterOpts/promoterOpts rather than mutated
// afterward. The core is exactly the part that was previously copied verbatim
// between cmd/veska/reindex.go and internal/cli/daemon/wire.go.
type ColdScanCore struct {
	Staging        *staging.Area
	Gate           *staging.Gate
	Ingester       *application.Ingester
	PromotionStore *sqlite.PromotionStore
	Promoter       *application.Promoter
}

// NewColdScanCore wires the cold-scan core over the given pools. reviewEnabled
// gates the optional WorkKindReview promotion lane (sqlite.WithReviewEnabled);
// pass false for the CLI path, which never enqueues review work. The
// caller-specific Ingester and Promoter collaborators (finding storage, check
// runner, added-lines seam, tracer) are forwarded as ingesterOpts/promoterOpts
// so the constructed core is fully wired and immutable.
func NewColdScanCore(pools *sqlite.Pools, reviewEnabled bool, ingesterOpts []application.IngesterOption, promoterOpts []application.PromoterOption) *ColdScanCore {
	area := staging.NewArea()
	gate := staging.NewGate(area)
	parser := treesitter.NewGoParser()
	ingester := application.NewIngester(parser, area, gate, ingesterOpts...)

	promotionStore := sqlite.NewPromotionStore(
		pools.Write,
		[]sqlite.PromotionSink{
			sqlite.NewFTSSink(),
			sqlite.NewEmbedRefSink(),
		},
		sqlite.WithReviewEnabled(reviewEnabled),
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
