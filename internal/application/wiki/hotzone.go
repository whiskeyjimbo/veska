// Package wiki contains the application-layer surfaces that render the
// generated developer wiki under docs/veska/.
//
// The hot_zone surface (m4.02) ranks source files by change risk: the
// product of how often a file changes (recent_change_frequency) and how
// much depends on it (blast_radius). The top-N files are rendered to a
// deterministic Markdown page so engineers and agents can see where
// change risk concentrates.
//
// The ranking service depends only on injected function types and the
// blastradius application service — never on internal/infrastructure —
// so the domain/application layering stays intact (make layercheck).
package wiki

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"

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
type Report struct {
	RepoID string    `json:"repo_id"`
	Branch string    `json:"branch"`
	Zones  []HotZone `json:"zones"`
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
//
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
		// git ChangeCounts returns repo-root-relative paths, but the
		// nodes table stores absolute file_paths. Without this join,
		// nodesInFile returns [] for every entry and every zone scores
		// 0, so .github/dependabot.yml ties with command.go (solov2-eb2).
		lookupPath := path
		if !filepath.IsAbs(lookupPath) {
			lookupPath = filepath.Join(repoRoot, path)
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
		zones = append(zones, HotZone{
			FilePath:              path,
			RecentChangeFrequency: freq,
			BlastRadius:           radius,
			Score:                 freq * radius,
		})
	}

	// Descending score; ascending path on ties — fully deterministic.
	sort.Slice(zones, func(i, j int) bool {
		if zones[i].Score != zones[j].Score {
			return zones[i].Score > zones[j].Score
		}
		return zones[i].FilePath < zones[j].FilePath
	})
	if len(zones) > s.topN {
		zones = zones[:s.topN]
	}
	return Report{RepoID: repoID, Branch: branch, Zones: zones}, nil
}
