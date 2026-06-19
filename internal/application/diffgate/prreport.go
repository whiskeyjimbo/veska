// SPDX-License-Identifier: AGPL-3.0-only

package diffgate

// PRReport is the advisory PR impact/risk report: a non-gating
// assembly of "what this diff touches and where it is risky". It is ADVISORY by
// construction - unlike the gate verdicts it has no Pass/Failures/ExitCode, and
// the CLI always exits 0 once it can emit one. Each section is sourced from an
// existing producer scoped to the diff's changed files; a producer that errors
// degrades its section (left empty) and appends a Note rather than failing the
// report. The report never blocks a merge - that is the whole point of the soft
// on-ramp (teams trust an advisory view before they let the graph gate).
// The index-ahead caveat that the gate family carries does NOT
// apply here: the report does not gate, so reflecting current index state is
// acceptable rather than a false-PASS.
type PRReport struct {
	RepoID       string             `json:"repo_id"`
	Branch       string             `json:"branch"`
	BaseRef      string             `json:"base_ref"`
	CandidateRef string             `json:"candidate_ref"`
	ChangedFiles []string           `json:"changed_files"`
	BlastRadius  BlastRadiusSection `json:"blast_radius"`
	ChangeRisk   []ChangeRiskFile   `json:"change_risk"`
	OpenFindings []ReportFinding    `json:"open_findings"`
	Untested     []UntestedSymbol   `json:"untested_changed"`
	// Notes records per-section degradation (a producer errored, or the repo is
	// not indexed) so the report is honest about what it could not assemble
	// without gating on it.
	Notes []string `json:"notes,omitempty"`
}

// BlastRadiusSection is the downstream impact of the diff: the nodes reachable
// from the changed files' symbols. NodeCount is the full reachable count;
// Entries is capped for readability (see ReportBlastEntryLimit), so NodeCount >
// len(Entries) means the list was display-truncated.
type BlastRadiusSection struct {
	SeedFiles int          `json:"seed_files"`
	NodeCount int          `json:"node_count"`
	Truncated bool         `json:"bfs_truncated"`
	Entries   []BlastEntry `json:"entries"`
}

// BlastEntry is one node in the diff's blast radius, with its BFS distance.
type BlastEntry struct {
	NodeID     string `json:"node_id"`
	SymbolPath string `json:"symbol_path"`
	FilePath   string `json:"file_path"`
	Kind       string `json:"kind"`
	Distance   int    `json:"distance"`
}

// ChangeRiskFile is one changed file's change-risk standing: the same
// recent-change-frequency × blast-radius score the hot_zone surface ranks by
// (wiki.HotZone), computed directly for the diff's files (no whole-repo top-N
// truncation, so every changed file gets a standing).
type ChangeRiskFile struct {
	FilePath              string `json:"file_path"`
	RecentChangeFrequency int    `json:"recent_change_frequency"`
	BlastRadius           int    `json:"blast_radius"`
	Score                 int    `json:"score"`
}

// ReportFinding is one open finding whose file is in the diff - a heads-up that
// a touched file carries a known issue. File-level (not node-level) so
// file-anchored findings are not dropped.
type ReportFinding struct {
	FindingID string `json:"finding_id"`
	Rule      string `json:"rule"`
	Severity  string `json:"severity"`
	FilePath  string `json:"file_path"`
	NodeID    string `json:"node_id,omitempty"`
	Message   string `json:"message"`
}

// ReportBlastEntryLimit caps how many blast-radius entries the report lists, to
// keep the advisory output readable on a large diff. The full count is reported
// in BlastRadiusSection.NodeCount.
const ReportBlastEntryLimit = 50
