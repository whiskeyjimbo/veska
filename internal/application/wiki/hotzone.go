// Package wiki contains the application-layer surfaces that render the
// generated developer wiki under
// The hot_zone surface (m4.02) ranks source files by change risk: the
// product of how often a file changes (recent_change_frequency) and how
// much depends on it (blast_radius). The top-N files are rendered to a
// deterministic Markdown page so engineers and agents can see where
// change risk concentrates.
// The ranking service depends only on injected function types and the
// blastradius application service — never on internal/infrastructure
// so the domain/application layering stays intact (make layercheck).
package wiki

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"time"

	"github.com/whiskeyjimbo/veska/internal/application/blastradius"
)

// ErrMissingDependency is returned by NewHotZoneService when a required
// dependency is nil. It wraps so callers can errors.Is against it.
var ErrMissingDependency = errors.New("wiki: missing required dependency")

// DefaultTopN is the number of files rendered when no TopN option is set.
const DefaultTopN = 20

// ChangeCountsFunc returns per-file commit counts within a look-back
// window. It mirrors git.ChangeCounts so the real adapter plugs in while
// tests pass a deterministic fake. The map is keyed by repoRoot-relative
// file path.
type ChangeCountsFunc func(ctx context.Context, repoRoot string) (map[string]int, error)

// NodesInFileFunc resolves a repoRoot-relative file path to the node IDs
// it contains. It mirrors ports.NodeLookup.NodesInFile so the MCP/SQLite
// adapter plugs in while tests pass a fake.
type NodesInFileFunc func(ctx context.Context, repoID, branch, filePath string) ([]string, error)

// HotZone is one ranked file: its change frequency, blast-radius factor,
// and the product score the ranking is sorted by.
type HotZone struct {
	FilePath              string `json:"file_path"`
	RecentChangeFrequency int    `json:"recent_change_frequency"`
	BlastRadius           int    `json:"blast_radius"`
	Score                 int    `json:"score"`
}

// Report is the ranked hot_zone surface: the top-N files ordered by
// descending score. It is the structure both the Markdown page and the
// eng_get_hot_zone MCP tool are built from, so the two never diverge.
// CandidatesScanned and CandidatesScored let callers distinguish two
// empty-Zones cases: no commits in the look-back window
// (CandidatesScanned == 0) vs. commits exist but every touched file
// scored 0 because it has no graph nodes (lockfiles, READMEs, …).
type Report struct {
	RepoID            string    `json:"repo_id"`
	Branch            string    `json:"branch"`
	Zones             []HotZone `json:"zones"`
	CandidatesScanned int       `json:"-"` // files touched in window
	CandidatesScored  int       `json:"-"` // files with score > 0
	// GeneratedAt is the wall-clock instant the report was rendered.
	// Populated by the wiki Handler immediately before rendering; the
	// service itself does not set it, so MCP responses can leave it zero
	// unless a caller wants staleness info.
	GeneratedAt time.Time `json:"generated_at,omitzero"`
}

// HotZoneService ranks files by change risk. It is stateless; the same
// instance is safe for concurrent callers.
type HotZoneService struct {
	changeCounts ChangeCountsFunc
	nodesInFile  NodesInFileFunc
	blast        *blastradius.Service
	topN         int
}

// Option configures a HotZoneService at construction time.
type Option func(*HotZoneService)

// WithTopN sets how many files the ranking retains. Non-positive values
// are ignored so the DefaultTopN stays in effect.
func WithTopN(n int) Option {
	return func(s *HotZoneService) {
		if n > 0 {
			s.topN = n
		}
	}
}

// NewHotZoneService constructs a HotZoneService. changeCounts, nodesInFile
// and blast are all required; a nil dependency yields an error wrapping
// ErrMissingDependency and a nil *HotZoneService.
func NewHotZoneService(changeCounts ChangeCountsFunc, nodesInFile NodesInFileFunc, blast *blastradius.Service, opts ...Option) (*HotZoneService, error) {
	if changeCounts == nil {
		return nil, fmt.Errorf("wiki.NewHotZoneService: changeCounts is nil: %w", ErrMissingDependency)
	}
	if nodesInFile == nil {
		return nil, fmt.Errorf("wiki.NewHotZoneService: nodesInFile is nil: %w", ErrMissingDependency)
	}
	if blast == nil {
		return nil, fmt.Errorf("wiki.NewHotZoneService: blast is nil: %w", ErrMissingDependency)
	}
	s := &HotZoneService{
		changeCounts: changeCounts,
		nodesInFile:  nodesInFile,
		blast:        blast,
		topN:         DefaultTopN,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s, nil
}

// Rank computes the hot_zone Report for (repoID, branch) over the working
// tree at repoRoot. Every file in the change-count map is scored by
// recent_change_frequency × blast_radius, where blast_radius is the entry
// count of running blastradius.Service.Of seeded with that file's nodes.
// Files are ranked by descending score; ties break by ascending file path
// so the output is byte-identical across runs given a fixed promoted
// state. The top-N files are retained.
func (s *HotZoneService) Rank(ctx context.Context, repoID, branch, repoRoot string) (Report, error) {
	counts, err := s.changeCounts(ctx, repoRoot)
	if err != nil {
		return Report{}, fmt.Errorf("wiki: change counts: %w", err)
	}

	zones := make([]HotZone, 0, len(counts))
	for path, freq := range counts {
		// git ChangeCounts and the nodes table now both key on the
		// repo-relative slash path, so the change-count
		// path feeds nodesInFile directly. An absolute path (defensive,
		// e.g. a non-git change source) is relativised to match.
		lookupPath := path
		if filepath.IsAbs(lookupPath) {
			if rel, rerr := filepath.Rel(repoRoot, lookupPath); rerr == nil {
				lookupPath = filepath.ToSlash(rel)
			}
		}
		seedIDs, err := s.nodesInFile(ctx, repoID, branch, lookupPath)
		if err != nil {
			return Report{}, fmt.Errorf("wiki: nodes in %s: %w", path, err)
		}
		radius := 0
		if len(seedIDs) > 0 {
			resp, err := s.blast.Of(ctx, repoID, branch, seedIDs, blastradius.Options{})
			if err != nil {
				return Report{}, fmt.Errorf("wiki: blast radius for %s: %w", path, err)
			}
			radius = len(resp.Entries)
		}
		// drop zones whose score is zero. A file that was
		// touched in-window but has zero downstream blast radius
		// (lockfiles, READMEs, generated assets, hand-edited go.mod
		// without graph nodes) is not "hot" by any meaningful
		// definition and crowds out genuinely hot files — or, on a
		// quiet repo with one churn file, produces a single
		// score=0 entry that contradicts the surface's promise.
		score := freq * radius
		if score == 0 {
			continue
		}
		zones = append(zones, HotZone{
			FilePath:              path,
			RecentChangeFrequency: freq,
			BlastRadius:           radius,
			Score:                 score,
		})
	}

	// Descending score; ascending path on ties — fully deterministic.
	sort.Slice(zones, func(i, j int) bool {
		if zones[i].Score != zones[j].Score {
			return zones[i].Score > zones[j].Score
		}
		return zones[i].FilePath < zones[j].FilePath
	})
	scored := len(zones)
	if len(zones) > s.topN {
		zones = zones[:s.topN]
	}
	return Report{
		RepoID:            repoID,
		Branch:            branch,
		Zones:             zones,
		CandidatesScanned: len(counts),
		CandidatesScored:  scored,
	}, nil
}
