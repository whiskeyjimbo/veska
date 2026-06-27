// SPDX-License-Identifier: AGPL-3.0-only

package diffgatecmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/whiskeyjimbo/veska/internal/application/diffgate"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// Output formats for the diff-gate subcommands. json is the historical default
// (the custom verdict envelope); sarif emits SARIF 2.1.0 for GitHub
// code-scanning. The set is validated at flag-parse time (addFormatFlag).
const (
	formatJSON  = "json"
	formatSARIF = "sarif"
)

// validFormat reports whether f is a recognized --format value.
func validFormat(f string) bool { return f == formatJSON || f == formatSARIF }

// SARIF 2.1.0 surface GitHub code-scanning ingests. Only the subset we populate
// is modeled; the schema is large but ingestion needs $schema + version + one
// run whose results each carry a ruleId, a message, and a physicalLocation with
// a repo-relative uri and a region.startLine. See GitHub's "SARIF support for
// code scanning" doc.
const (
	sarifSchemaURI = "https://json.schemastore.org/sarif-2.1.0.json"
	sarifVersion   = "2.1.0"
	// sarifInformationURI points a Security-tab reader at the gate's docs.
	sarifInformationURI = "https://github.com/whiskeyjimbo/veska"
	// sarifLevelError marks every gate finding as an error: a gate FAIL blocks
	// the merge, so the matching alert is error-level, not a warning.
	sarifLevelError = "error"
)

type sarifLog struct {
	Schema  string     `json:"$schema"`
	Version string     `json:"version"`
	Runs    []sarifRun `json:"runs"`
}

type sarifRun struct {
	Tool sarifTool `json:"tool"`
	// AutomationDetails.id gives each gate's run an explicit, stable, distinct
	// analysis identity. GitHub prefers it over the upload-sarif `category`
	// param, so 5 gate runs submitted together register as 5 independent
	// analyses (no collision) and a PASS run still clears the prior alert
	// because the id is stable run-over-run.
	AutomationDetails sarifAutomationDetails `json:"automationDetails"`
	Results           []sarifResult          `json:"results"`
}

type sarifAutomationDetails struct {
	ID string `json:"id"`
}

type sarifTool struct {
	Driver sarifDriver `json:"driver"`
}

type sarifDriver struct {
	Name           string      `json:"name"`
	InformationURI string      `json:"informationUri,omitempty"`
	Rules          []sarifRule `json:"rules"`
}

// sarifRule is a reportingDescriptor. id reuses the gate's stable Fail* string
// so the SARIF ruleId matches the JSON verdict's failing-check name.
type sarifRule struct {
	ID               string    `json:"id"`
	Name             string    `json:"name,omitempty"`
	ShortDescription sarifText `json:"shortDescription"`
	FullDescription  sarifText `json:"fullDescription"`
	Help             sarifText `json:"help"`
}

type sarifResult struct {
	RuleID    string          `json:"ruleId"`
	Level     string          `json:"level"`
	Message   sarifText       `json:"message"`
	Locations []sarifLocation `json:"locations"`
}

type sarifLocation struct {
	PhysicalLocation sarifPhysicalLocation `json:"physicalLocation"`
}

type sarifPhysicalLocation struct {
	ArtifactLocation sarifArtifactLocation `json:"artifactLocation"`
	Region           sarifRegion           `json:"region"`
}

type sarifArtifactLocation struct {
	URI string `json:"uri"`
}

type sarifRegion struct {
	StartLine int `json:"startLine"`
}

type sarifText struct {
	Text string `json:"text"`
}

// sarifRuleCatalog maps each gate failure-name (the stable Fail* constants,
// reused verbatim as SARIF ruleIds) to its reporting descriptor. Centralized so
// every gate's run declares consistent rule metadata; a run declares the subset
// it can emit (see gate*SarifLog), which is also what lets a PASS run clear a
// previously-reported alert for that rule.
var sarifRuleCatalog = map[string]sarifRule{
	diffgate.FailBreakingAPIChange: {
		ID:               diffgate.FailBreakingAPIChange,
		Name:             "BreakingAPIChange",
		ShortDescription: sarifText{Text: "Breaking change to an exported symbol's signature"},
		FullDescription:  sarifText{Text: "The candidate changed the signature shape (name, parameters, or result) of an exported symbol - a breaking public-surface change importers cannot absorb without edits."},
		Help:             sarifText{Text: "Restore the exported signature, or treat this as an intentional breaking change (e.g. a major version bump)."},
	},
	diffgate.FailRemovedAPISymbol: {
		ID:               diffgate.FailRemovedAPISymbol,
		Name:             "RemovedAPISymbol",
		ShortDescription: sarifText{Text: "Exported symbol removed or renamed"},
		FullDescription:  sarifText{Text: "An exported symbol present at the base ref is absent from the candidate (removed, renamed, or unexported) - a breaking change for any importer of that name."},
		Help:             sarifText{Text: "Keep the exported name (add a deprecated alias if renaming), or treat the removal as an intentional breaking change."},
	},
	"new_clone": {
		ID:               "new_clone",
		Name:             "NewExactClone",
		ShortDescription: sarifText{Text: "Net-new exact-duplicate code"},
		FullDescription:  sarifText{Text: "The candidate introduced a byte-identical copy (content-hash equality) of code it did not already duplicate at the base ref."},
		Help:             sarifText{Text: "Extract the duplicated logic into a shared symbol instead of copying it."},
	},
	diffgate.FailClonesUnchecked: {
		ID:               diffgate.FailClonesUnchecked,
		Name:             "ClonesUnchecked",
		ShortDescription: sarifText{Text: "Exact-clone gate could not run"},
		FullDescription:  sarifText{Text: "The exact-clone gate degraded to unchecked (the base graph could not be read), so the gate fails closed rather than risk a false pass."},
		Help:             sarifText{Text: "Re-index the base graph, then re-run the gate."},
	},
	diffgate.FailNewCycle: {
		ID:               diffgate.FailNewCycle,
		Name:             "NewDependencyCycle",
		ShortDescription: sarifText{Text: "Net-new dependency cycle"},
		FullDescription:  sarifText{Text: "The candidate introduced a dependency cycle (a strongly-connected component of two or more symbols over CALLS/IMPORTS edges) absent at the base ref."},
		Help:             sarifText{Text: "Break the cycle by inverting a dependency or extracting the shared contract into a third symbol."},
	},
	diffgate.FailNewSecretLeak: {
		ID:               diffgate.FailNewSecretLeak,
		Name:             "NewSecretLeak",
		ShortDescription: sarifText{Text: "Net-new hardcoded secret"},
		FullDescription:  sarifText{Text: "The candidate added a line containing a secret-shaped value (API key, token, private key) absent at the base ref."},
		Help:             sarifText{Text: "Remove the secret, rotate it, and load it from configuration or a secrets manager instead."},
	},
	diffgate.FailNewVulnDep: {
		ID:               diffgate.FailNewVulnDep,
		Name:             "NewVulnerableDependency",
		ShortDescription: sarifText{Text: "Net-new vulnerable dependency"},
		FullDescription:  sarifText{Text: "The candidate added (or bumped into range of) a dependency with a known advisory that was not flagged at the base ref."},
		Help:             sarifText{Text: "Upgrade to a patched version, or remove the dependency."},
	},
	diffgate.FailVulnUnchecked: {
		ID:               diffgate.FailVulnUnchecked,
		Name:             "VulnUnchecked",
		ShortDescription: sarifText{Text: "Vulnerable-dependency scan could not run"},
		FullDescription:  sarifText{Text: "A vulnerability source is configured but the advisory cache was empty or unreadable, so the vuln dimension degraded to unchecked and the gate fails closed."},
		Help:             sarifText{Text: "Refresh the advisory cache, then re-run the gate."},
	},
	diffgate.FailUntestedChanged: {
		ID:               diffgate.FailUntestedChanged,
		Name:             "UntestedChangedSymbol",
		ShortDescription: sarifText{Text: "Changed prod symbol no test reaches"},
		FullDescription:  sarifText{Text: "The candidate changed or added a production symbol that no test reaches (a CALLS-edge coverage proxy, not real coverage data) - a regression the test bar misses because the code still compiles."},
		Help:             sarifText{Text: "Add or extend a test that exercises the changed symbol."},
	},
}

// rulesFor returns the reporting descriptors for the given rule ids, in the
// order supplied, skipping any not in the catalog. A run declares its full
// possible rule set (not just the ids it emitted this time) so a later PASS run
// with no results clears the prior alert for that rule.
func rulesFor(ids ...string) []sarifRule {
	out := make([]sarifRule, 0, len(ids))
	for _, id := range ids {
		if r, ok := sarifRuleCatalog[id]; ok {
			out = append(out, r)
		}
	}
	return out
}

// newSarifLog assembles a single-run SARIF log for one gate. driverName is the
// per-gate tool name (e.g. veska-diff-gate/api) so each gate's alerts live in
// their own namespace and a passing gate only clears its own. results may be
// empty (a PASS): the declared rules still let code-scanning resolve fixed
// alerts. A nil results slice is normalized to an empty array - SARIF requires
// the results key to be present.
func newSarifLog(driverName string, rules []sarifRule, results []sarifResult) sarifLog {
	if results == nil {
		results = []sarifResult{}
	}
	return sarifLog{
		Schema:  sarifSchemaURI,
		Version: sarifVersion,
		Runs: []sarifRun{{
			Tool: sarifTool{Driver: sarifDriver{
				Name:           driverName,
				InformationURI: sarifInformationURI,
				Rules:          rules,
			}},
			AutomationDetails: sarifAutomationDetails{ID: driverName},
			Results:           results,
		}},
	}
}

// emitSarif writes the SARIF log as indented JSON, mirroring the JSON-verdict
// emitters so a CI consumer always gets a parseable document on stdout.
func emitSarif(out io.Writer, log sarifLog) error {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	if err := enc.Encode(log); err != nil {
		return fmt.Errorf("diff-gate: encode sarif: %w", err)
	}
	return nil
}

// nodeLocator resolves a node_id to its source line for a SARIF region. It
// batches a single NodeLookup over all ids a gate references. A node absent
// from the (base) index - notably a symbol ADDED by the candidate, which never
// existed at base - resolves to ok=false; callers fall back to a file-level
// anchor (startLine 1) so every result still carries a valid location.
type nodeLocator struct {
	byID map[string]ports.NodeMeta
}

// newNodeLocator batches one LookupNodes call over ids. A lookup error yields an
// empty locator (every line() returns ok=false → file-level fallback) rather
// than failing the gate: a missing line degrades alert precision, not validity.
func newNodeLocator(ctx context.Context, nl ports.NodeLookup, repoID, branch string, ids []string) nodeLocator {
	loc := nodeLocator{byID: map[string]ports.NodeMeta{}}
	if nl == nil || len(ids) == 0 {
		return loc
	}
	meta, err := nl.LookupNodes(ctx, repoID, branch, ids)
	if err != nil {
		return loc
	}
	for _, nm := range meta {
		loc.byID[nm.NodeID] = nm
	}
	return loc
}

// line returns the 1-indexed start line for nodeID, or ok=false when the node
// is not in the (base) index or carries no line.
func (l nodeLocator) line(nodeID string) (int, bool) {
	nm, ok := l.byID[nodeID]
	if !ok || nm.LineStart <= 0 {
		return 0, false
	}
	return nm.LineStart, true
}

// meta returns the full NodeMeta for nodeID. Used by the untested gate, whose
// verdict carries no FilePath of its own, so the locator (built over the
// candidate graph) supplies both file and line.
func (l nodeLocator) meta(nodeID string) (ports.NodeMeta, bool) {
	nm, ok := l.byID[nodeID]
	return nm, ok
}

// physLoc builds a physicalLocation. A line <= 0 (no source line, or an added
// node the base index can't resolve) becomes a file-level anchor at startLine 1
// - always valid SARIF, since uri + region.startLine are what code-scanning
// needs. uri is the repo-relative path; callers guarantee it is non-empty.
func physLoc(uri string, line int) sarifPhysicalLocation {
	if line <= 0 {
		line = 1
	}
	return sarifPhysicalLocation{
		ArtifactLocation: sarifArtifactLocation{URI: uri},
		Region:           sarifRegion{StartLine: line},
	}
}

// at is a one-location convenience for results anchored to a single position.
func at(uri string, line int) []sarifLocation {
	return []sarifLocation{{PhysicalLocation: physLoc(uri, line)}}
}

// ---- per-gate driver names + rule sets ----

const (
	driverAPI      = "veska-diff-gate/api"
	driverClones   = "veska-diff-gate/clones"
	driverCycles   = "veska-diff-gate/cycles"
	driverSecurity = "veska-diff-gate/security"
	driverUntested = "veska-diff-gate/untested"
)

// ---- api ----

// apiNodeIDs collects the node ids an api verdict references, for one batched
// base-index line lookup. Breaking-change and removed nodes are pre-existing
// (changed or base-ref) symbols, so they resolve in the base index → line-level.
func apiNodeIDs(v diffgate.APIVerdict) []string {
	ids := make([]string, 0, len(v.BreakingChanges)+len(v.RemovedSymbols))
	for _, c := range v.BreakingChanges {
		ids = append(ids, c.NodeID)
	}
	for _, r := range v.RemovedSymbols {
		ids = append(ids, r.NodeID)
	}
	return ids
}

// apiSarifLog maps an APIVerdict to its SARIF log. One result per breaking
// change / removal; the verdict's FilePath is the uri and the base-index line
// the region (file-level fallback on a lookup miss).
func apiSarifLog(v diffgate.APIVerdict, loc nodeLocator) sarifLog {
	results := make([]sarifResult, 0, len(v.BreakingChanges)+len(v.RemovedSymbols))
	for _, c := range v.BreakingChanges {
		line, _ := loc.line(c.NodeID)
		results = append(results, sarifResult{
			RuleID:    diffgate.FailBreakingAPIChange,
			Level:     sarifLevelError,
			Message:   sarifText{Text: fmt.Sprintf("Breaking API change: exported %s %s changed signature from %q to %q.", c.Kind, c.SymbolPath, c.PrevSig, c.NewSig)},
			Locations: at(c.FilePath, line),
		})
	}
	for _, r := range v.RemovedSymbols {
		line, _ := loc.line(r.NodeID)
		results = append(results, sarifResult{
			RuleID:    diffgate.FailRemovedAPISymbol,
			Level:     sarifLevelError,
			Message:   sarifText{Text: fmt.Sprintf("Removed API symbol: exported %s %s is absent from the candidate (removed, renamed, or unexported).", r.Kind, r.SymbolPath)},
			Locations: at(r.FilePath, line),
		})
	}
	return newSarifLog(driverAPI, rulesFor(diffgate.FailBreakingAPIChange, diffgate.FailRemovedAPISymbol), results)
}

// ---- clones ----

// cloneNodeIDs collects every clone-member node id. A NEW clone member is often
// an ADDED node absent from the base index, so its line lookup misses and the
// result falls back to the member's FilePath (file-level).
func cloneNodeIDs(v diffgate.CloneVerdict) []string {
	var ids []string
	for _, g := range v.NewClones {
		for _, m := range g.Members {
			ids = append(ids, m.NodeID)
		}
	}
	return ids
}

// cloneSarifLog maps a CloneVerdict to its SARIF log: one result per net-new
// clone GROUP, with every member as a location so the alert points at all the
// duplicate sites.
func cloneSarifLog(v diffgate.CloneVerdict, loc nodeLocator) sarifLog {
	results := make([]sarifResult, 0, len(v.NewClones))
	for _, g := range v.NewClones {
		locs := make([]sarifLocation, 0, len(g.Members))
		paths := make([]string, 0, len(g.Members))
		for _, m := range g.Members {
			line, _ := loc.line(m.NodeID)
			locs = append(locs, sarifLocation{PhysicalLocation: physLoc(m.FilePath, line)})
			paths = append(paths, m.SymbolPath)
		}
		results = append(results, sarifResult{
			RuleID:    "new_clone",
			Level:     sarifLevelError,
			Message:   sarifText{Text: fmt.Sprintf("Net-new exact clone: %d identical copies (%s).", len(g.Members), strings.Join(paths, ", "))},
			Locations: locs,
		})
	}
	return newSarifLog(driverClones, rulesFor("new_clone", diffgate.FailClonesUnchecked), results)
}

// ---- cycles ----

// cycleNodeIDs collects every cycle-member node id. As with clones, a member
// newly added by the candidate misses the base-index lookup → file-level.
func cycleNodeIDs(v diffgate.CycleVerdict) []string {
	var ids []string
	for _, g := range v.NewCycles {
		for _, m := range g.Members {
			ids = append(ids, m.NodeID)
		}
	}
	return ids
}

// cycleSarifLog maps a CycleVerdict to its SARIF log: one result per net-new
// cycle, every member symbol as a location.
func cycleSarifLog(v diffgate.CycleVerdict, loc nodeLocator) sarifLog {
	results := make([]sarifResult, 0, len(v.NewCycles))
	for _, g := range v.NewCycles {
		locs := make([]sarifLocation, 0, len(g.Members))
		paths := make([]string, 0, len(g.Members))
		for _, m := range g.Members {
			line, _ := loc.line(m.NodeID)
			locs = append(locs, sarifLocation{PhysicalLocation: physLoc(m.FilePath, line)})
			paths = append(paths, m.SymbolPath)
		}
		results = append(results, sarifResult{
			RuleID:    diffgate.FailNewCycle,
			Level:     sarifLevelError,
			Message:   sarifText{Text: fmt.Sprintf("Net-new dependency cycle among: %s.", strings.Join(paths, " -> "))},
			Locations: locs,
		})
	}
	return newSarifLog(driverCycles, rulesFor(diffgate.FailNewCycle), results)
}

// ---- security ----

// securitySarifLog maps a SecurityVerdict to its SARIF log. Security findings
// are FILE-LEVEL (startLine 1): a secret_leak anchors to its file and a
// vulnerable_dependency to its manifest - neither carries a usable line on the
// domain.Finding (the matched secret line survives in the message text, not the
// anchor). The full finding message is preserved so a reviewer still sees it.
func securitySarifLog(v diffgate.SecurityVerdict) sarifLog {
	results := make([]sarifResult, 0, len(v.NewSecretLeaks)+len(v.NewVulnDeps))
	for _, f := range v.NewSecretLeaks {
		results = append(results, securityResult(diffgate.FailNewSecretLeak, f))
	}
	for _, f := range v.NewVulnDeps {
		results = append(results, securityResult(diffgate.FailNewVulnDep, f))
	}
	return newSarifLog(driverSecurity, rulesFor(diffgate.FailNewSecretLeak, diffgate.FailNewVulnDep, diffgate.FailVulnUnchecked), results)
}

// securityResult builds a file-level result for a security finding. A finding
// with no file anchor still needs a uri; the repo-relative manifest/file path
// is on the finding, but an empty path degrades to "." (the repo root) so the
// result stays valid SARIF.
func securityResult(ruleID string, f diffgate.SecurityFinding) sarifResult {
	uri := f.FilePath
	if uri == "" {
		uri = "."
	}
	return sarifResult{
		RuleID:    ruleID,
		Level:     sarifLevelError,
		Message:   sarifText{Text: f.Message},
		Locations: at(uri, 1),
	}
}

// ---- untested ----

// untestedSarifLog maps a CoverageVerdict to its SARIF log. UntestedSymbol
// carries only a node_id (no FilePath), and the flagged symbols are often ADDED
// by the candidate - absent from the base index - so the locator MUST be built
// over the candidate graph (the re-promoted clone in untestedInChangedFiles),
// not the base pool. Both uri and line come from that locator's NodeMeta; an
// unresolved node degrades to a "." file anchor so the result stays valid.
func untestedSarifLog(v diffgate.CoverageVerdict, loc nodeLocator) sarifLog {
	results := make([]sarifResult, 0, len(v.UntestedChanged))
	for _, s := range v.UntestedChanged {
		uri, line := ".", 0
		if nm, ok := loc.meta(s.NodeID); ok {
			if nm.FilePath != "" {
				uri = nm.FilePath
			}
			line = nm.LineStart
		}
		results = append(results, sarifResult{
			RuleID:    diffgate.FailUntestedChanged,
			Level:     sarifLevelError,
			Message:   sarifText{Text: s.Message},
			Locations: at(uri, line),
		})
	}
	return newSarifLog(driverUntested, rulesFor(diffgate.FailUntestedChanged), results)
}
