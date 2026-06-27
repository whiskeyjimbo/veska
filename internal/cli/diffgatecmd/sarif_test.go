// SPDX-License-Identifier: AGPL-3.0-only

package diffgatecmd

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/application/diffgate"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// assertIngestibleSARIF asserts the structural invariants GitHub code-scanning
// requires: the schema URI + version, a named driver with a non-empty rules
// array, and every result carrying a ruleId, a message, and a physicalLocation
// with a non-empty repo-relative uri and a region.startLine >= 1. It also checks
// that every emitted result's ruleId is one the run DECLARED (else code-scanning
// drops it).
func assertIngestibleSARIF(t *testing.T, log sarifLog) {
	t.Helper()
	if log.Schema != sarifSchemaURI {
		t.Errorf("schema = %q, want %q", log.Schema, sarifSchemaURI)
	}
	if log.Version != sarifVersion {
		t.Errorf("version = %q, want %q", log.Version, sarifVersion)
	}
	if len(log.Runs) != 1 {
		t.Fatalf("runs = %d, want 1", len(log.Runs))
	}
	run := log.Runs[0]
	if run.Tool.Driver.Name == "" {
		t.Error("driver name is empty")
	}
	if len(run.Tool.Driver.Rules) == 0 {
		t.Error("driver declares no rules (a PASS run still needs them to clear fixed alerts)")
	}
	if run.AutomationDetails.ID == "" {
		t.Error("run.automationDetails.id is empty (needed for a distinct, stable analysis identity)")
	}
	declared := map[string]bool{}
	for _, r := range run.Tool.Driver.Rules {
		if r.ID == "" || r.ShortDescription.Text == "" || r.FullDescription.Text == "" {
			t.Errorf("rule %+v missing id/short/full description", r)
		}
		declared[r.ID] = true
	}
	for i, res := range run.Results {
		if res.RuleID == "" {
			t.Errorf("result %d has empty ruleId", i)
		}
		if !declared[res.RuleID] {
			t.Errorf("result %d ruleId %q is not declared by the run", i, res.RuleID)
		}
		if res.Message.Text == "" {
			t.Errorf("result %d has empty message", i)
		}
		if len(res.Locations) == 0 {
			t.Errorf("result %d has no locations (uri is required)", i)
		}
		for j, loc := range res.Locations {
			if loc.PhysicalLocation.ArtifactLocation.URI == "" {
				t.Errorf("result %d location %d has empty uri", i, j)
			}
			if loc.PhysicalLocation.Region.StartLine < 1 {
				t.Errorf("result %d location %d startLine = %d, want >= 1", i, j, loc.PhysicalLocation.Region.StartLine)
			}
		}
	}
	// The emitted document must round-trip as JSON (the $schema key included).
	var buf bytes.Buffer
	if err := emitSarif(&buf, log); err != nil {
		t.Fatalf("emitSarif: %v", err)
	}
	var back map[string]any
	if err := json.Unmarshal(buf.Bytes(), &back); err != nil {
		t.Fatalf("emitted SARIF is not valid JSON: %v", err)
	}
	if _, ok := back["$schema"]; !ok {
		t.Error("emitted SARIF missing $schema key")
	}
}

// fakeLocator builds a nodeLocator that resolves the given node ids to a line,
// for testing line-level vs file-level fallback without a DB.
func fakeLocator(lines map[string]int) nodeLocator {
	byID := map[string]ports.NodeMeta{}
	for id, ln := range lines {
		byID[id] = ports.NodeMeta{NodeID: id, LineStart: ln}
	}
	return nodeLocator{byID: byID}
}

func TestAPISarifLog(t *testing.T) {
	v := diffgate.APIVerdict{
		Pass: false,
		BreakingChanges: []diffgate.APIChange{
			{NodeID: "n1", FilePath: "pkg/a.go", SymbolPath: "A", Kind: "function", PrevSig: "A()", NewSig: "A(x int)"},
		},
		RemovedSymbols: []diffgate.APIRemoval{
			{NodeID: "n2", FilePath: "pkg/b.go", SymbolPath: "B", Kind: "type"},
		},
	}
	loc := fakeLocator(map[string]int{"n1": 12, "n2": 30})
	log := apiSarifLog(v, loc)
	assertIngestibleSARIF(t, log)

	res := log.Runs[0].Results
	if len(res) != 2 {
		t.Fatalf("results = %d, want 2", len(res))
	}
	// Line-level: the locator hit, so the region is the symbol's line, not 1.
	if got := res[0].Locations[0].PhysicalLocation.Region.StartLine; got != 12 {
		t.Errorf("breaking-change startLine = %d, want 12", got)
	}
	if res[0].RuleID != diffgate.FailBreakingAPIChange {
		t.Errorf("ruleId = %q, want %q", res[0].RuleID, diffgate.FailBreakingAPIChange)
	}
}

func TestAPISarifLogPassEmptyResults(t *testing.T) {
	log := apiSarifLog(diffgate.APIVerdict{Pass: true}, nodeLocator{})
	assertIngestibleSARIF(t, log)
	if len(log.Runs[0].Results) != 0 {
		t.Errorf("PASS verdict must yield zero results, got %d", len(log.Runs[0].Results))
	}
	// Rules are still declared so a fixed alert clears.
	if len(log.Runs[0].Tool.Driver.Rules) == 0 {
		t.Error("PASS run must still declare its rules")
	}
}

func TestCloneSarifLogFileLevelFallback(t *testing.T) {
	v := diffgate.CloneVerdict{
		Pass:    false,
		Checked: true,
		NewClones: []diffgate.CloneGroup{{
			ContentHash: "deadbeef",
			Members: []diffgate.CloneMember{
				{NodeID: "added1", FilePath: "pkg/x.go", SymbolPath: "X"},
				{NodeID: "added2", FilePath: "pkg/y.go", SymbolPath: "Y"},
			},
		}},
	}
	// Empty locator: both members are newly-added nodes the base index can't
	// resolve, so each must fall back to a file-level anchor (startLine 1).
	log := cloneSarifLog(v, nodeLocator{})
	assertIngestibleSARIF(t, log)
	res := log.Runs[0].Results
	if len(res) != 1 {
		t.Fatalf("results = %d, want 1 (one per clone group)", len(res))
	}
	if len(res[0].Locations) != 2 {
		t.Fatalf("clone-group locations = %d, want 2", len(res[0].Locations))
	}
	for _, loc := range res[0].Locations {
		if loc.PhysicalLocation.Region.StartLine != 1 {
			t.Errorf("file-level fallback startLine = %d, want 1", loc.PhysicalLocation.Region.StartLine)
		}
	}
}

func TestCycleSarifLog(t *testing.T) {
	v := diffgate.CycleVerdict{
		Pass: false,
		NewCycles: []diffgate.CycleGroup{{
			Members: []diffgate.CycleMember{
				{NodeID: "c1", FilePath: "pkg/a.go", SymbolPath: "A"},
				{NodeID: "c2", FilePath: "pkg/b.go", SymbolPath: "B"},
			},
		}},
	}
	log := cycleSarifLog(v, fakeLocator(map[string]int{"c1": 5}))
	assertIngestibleSARIF(t, log)
	res := log.Runs[0].Results
	if len(res) != 1 {
		t.Fatalf("results = %d, want 1", len(res))
	}
	if res[0].RuleID != diffgate.FailNewCycle {
		t.Errorf("ruleId = %q, want %q", res[0].RuleID, diffgate.FailNewCycle)
	}
}

func TestSecuritySarifLogFileLevel(t *testing.T) {
	v := diffgate.SecurityVerdict{
		Pass: false,
		NewSecretLeaks: []diffgate.SecurityFinding{
			{Rule: "secret_leak", FindingID: "f1", Message: "secret detected by rule \"aws\" at line 7: AKIA...", FilePath: "config/secrets.yml"},
		},
		NewVulnDeps: []diffgate.SecurityFinding{
			{Rule: "vulnerable_dependency", FindingID: "f2", Message: "CVE-2024-1 in foo@1.2.3", FilePath: "go.mod"},
		},
		VulnApplicable: true,
		VulnChecked:    true,
	}
	log := securitySarifLog(v)
	assertIngestibleSARIF(t, log)
	res := log.Runs[0].Results
	if len(res) != 2 {
		t.Fatalf("results = %d, want 2", len(res))
	}
	// Security is file-level: startLine 1, uri = the finding's file. The matched
	// line survives in the message text.
	for _, r := range res {
		if r.Locations[0].PhysicalLocation.Region.StartLine != 1 {
			t.Errorf("security startLine = %d, want 1 (file-level)", r.Locations[0].PhysicalLocation.Region.StartLine)
		}
	}
	if !strings.Contains(res[0].Message.Text, "at line 7") {
		t.Errorf("secret message should preserve the matched line, got %q", res[0].Message.Text)
	}
}

func TestSecuritySarifLogUnanchoredURIFallback(t *testing.T) {
	v := diffgate.SecurityVerdict{
		Pass:           false,
		NewSecretLeaks: []diffgate.SecurityFinding{{Rule: "secret_leak", FindingID: "f1", Message: "leak", FilePath: ""}},
	}
	log := securitySarifLog(v)
	// A finding with no file anchor must still produce a valid (non-empty) uri.
	assertIngestibleSARIF(t, log)
	if got := log.Runs[0].Results[0].Locations[0].PhysicalLocation.ArtifactLocation.URI; got != "." {
		t.Errorf("unanchored uri = %q, want %q", got, ".")
	}
}

func TestUntestedSarifLog(t *testing.T) {
	v := diffgate.CoverageVerdict{
		Pass: false,
		UntestedChanged: []diffgate.UntestedSymbol{
			{NodeID: "added", Message: "changed symbol DoThing has no test"},
			{NodeID: "missing", Message: "changed symbol Other has no test"},
		},
	}
	// "added" resolves via the candidate-graph locator (file + line); "missing"
	// is absent from the locator and must fall back to a "." anchor.
	loc := nodeLocator{byID: map[string]ports.NodeMeta{
		"added": {NodeID: "added", FilePath: "pkg/do.go", LineStart: 42},
	}}
	log := untestedSarifLog(v, loc)
	assertIngestibleSARIF(t, log)
	res := log.Runs[0].Results
	if len(res) != 2 {
		t.Fatalf("results = %d, want 2", len(res))
	}
	if got := res[0].Locations[0].PhysicalLocation; got.ArtifactLocation.URI != "pkg/do.go" || got.Region.StartLine != 42 {
		t.Errorf("resolved location = %+v, want pkg/do.go:42", got)
	}
	if got := res[1].Locations[0].PhysicalLocation; got.ArtifactLocation.URI != "." || got.Region.StartLine != 1 {
		t.Errorf("unresolved fallback = %+v, want .:1", got)
	}
}

func TestValidFormat(t *testing.T) {
	for _, f := range []string{formatJSON, formatSARIF} {
		if !validFormat(f) {
			t.Errorf("validFormat(%q) = false, want true", f)
		}
	}
	for _, f := range []string{"", "xml", "JSON"} {
		if validFormat(f) {
			t.Errorf("validFormat(%q) = true, want false", f)
		}
	}
}
