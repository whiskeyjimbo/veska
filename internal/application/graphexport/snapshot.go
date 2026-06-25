// SPDX-License-Identifier: AGPL-3.0-only

// Package graphexport serializes a repo's code graph to a deterministic,
// shareable JSON snapshot. The snapshot is the contract consumed by the
// read-only web viewer (veska graph serve) and is safe to commit so teammates
// skip indexing. Determinism is a hard requirement, not a nicety: re-running
// the export on an unchanged repo yields byte-identical output, so the
// committed-to-git story produces no spurious diffs.
package graphexport

import (
	"encoding/json"
	"fmt"
)

// SchemaVersion is the snapshot envelope version. Bump on any
// backward-incompatible change to the JSON shape so the viewer can refuse or
// adapt to formats it does not understand.
const SchemaVersion = 1

// Snapshot is the top-level serialized graph. All slices are emitted in a
// stable, content-derived order (nodes by id, edges by edge id, the rest in
// the deterministic order their producing service already guarantees) so the
// marshaled bytes are reproducible.
type Snapshot struct {
	SchemaVersion int               `json:"schema_version"`
	RepoID        string            `json:"repo_id"`
	Branch        string            `json:"branch"`
	Nodes         []NodeEntry       `json:"nodes"`
	Edges         []EdgeEntry       `json:"edges"`
	HotZones      []HotZoneEntry    `json:"hot_zones"`
	EntryPoints   []EntryPointEntry `json:"entry_points"`
	Dependencies  []DependencyEntry `json:"dependencies"`
}

// NodeEntry is the snapshot projection of a domain.Node. Paths are
// repo-root-relative (no home/username leakage). Summary is always populated:
// the stored ShortSummary when present, else the deterministic
// HeuristicSummary fallback - the same projection rule the MCP node DTO uses.
// External (vendored / module-cache) nodes are excluded from the snapshot;
// they are represented through Dependencies instead.
type NodeEntry struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	Path       string `json:"path"`
	LineStart  int    `json:"line_start,omitempty"`
	LineEnd    int    `json:"line_end,omitempty"`
	Signature  string `json:"signature,omitempty"`
	Language   string `json:"language,omitempty"`
	Exported   bool   `json:"exported,omitempty"`
	Summary    string `json:"summary"`
	RawContent string `json:"raw_content,omitempty"`
}

// EdgeEntry is the snapshot projection of a domain.Edge. Unresolved edges
// (Confidence == Unresolved, i.e. proposed SIMILAR_TO links awaiting review)
// are excluded; the snapshot carries confirmed structural relationships only.
type EdgeEntry struct {
	ID         string   `json:"id"`
	Src        string   `json:"src"`
	Tgt        string   `json:"tgt"`
	Kind       string   `json:"kind"`
	Confidence string   `json:"confidence"`
	Resolved   bool     `json:"resolved"`
	SourceLine int      `json:"source_line,omitempty"`
	Score      *float32 `json:"score,omitempty"`
}

// HotZoneEntry is the snapshot projection of a wiki hot-zone ranking row.
type HotZoneEntry struct {
	FilePath              string `json:"file_path"`
	RecentChangeFrequency int    `json:"recent_change_frequency"`
	BlastRadius           int    `json:"blast_radius"`
	Score                 int    `json:"score"`
}

// EntryPointEntry is the snapshot projection of a wiki entry-point row.
type EntryPointEntry struct {
	SymbolName      string `json:"symbol_name"`
	FilePath        string `json:"file_path"`
	Kind            string `json:"kind"`
	InboundCount    int    `json:"inbound_count"`
	Exported        bool   `json:"exported"`
	HasAdjacentTest bool   `json:"has_adjacent_test"`
}

// DependencyEntry is the snapshot projection of an external module the repo
// calls into, mirroring the eng_list_dependencies listing.
type DependencyEntry struct {
	Module       string               `json:"module"`
	Version      string               `json:"version,omitempty"`
	Language     string               `json:"language"`
	UsageCount   int                  `json:"usage_count"`
	ImportCount  int                  `json:"import_count,omitempty"`
	TopCallSites []DependencyCallSite `json:"top_call_sites,omitempty"`
}

// DependencyCallSite is a sampled call site for a DependencyEntry.
type DependencyCallSite struct {
	SrcNodeID  string `json:"src_node_id"`
	SymbolPath string `json:"symbol_path"`
}

// Marshal renders the snapshot to indented JSON. Encoding is the determinism
// boundary: given a Snapshot whose slices are already in stable order, the
// bytes are reproducible. Indentation is fixed (two spaces) so a committed
// snapshot diffs cleanly. A trailing newline is appended for POSIX-friendly
// files and stable git blobs.
func Marshal(snap Snapshot) ([]byte, error) {
	b, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("graphexport: marshal snapshot: %w", err)
	}
	return append(b, '\n'), nil
}
