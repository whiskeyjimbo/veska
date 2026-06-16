package diffgate_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/application/checks"
	"github.com/whiskeyjimbo/veska/internal/application/diffgate"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// fakes

// fakeSecrets emits a secret_leak finding for every added line containing the
// literal "SECRET", anchored on the file with a line discriminator.
func fakeSecrets(_ context.Context, in checks.Input) ([]*domain.Finding, error) {
	var out []*domain.Finding
	for path, lines := range in.AddedLines {
		for _, l := range lines {
			if strings.Contains(l.Text, "SECRET") {
				f, err := domain.NewFinding(domain.FindingSpec{
					RepoID: in.RepoID, Branch: in.Branch,
					Severity: domain.SeverityHigh, Layer: domain.LayerSecurity,
					Rule: "secret_leak", Message: "secret at " + path,
				}, domain.WithFileAnchor(path), domain.WithFindingKey(in.RepoID+path))
				if err != nil {
					return nil, err
				}
				out = append(out, f)
			}
		}
	}
	return out, nil
}

// fakeDepsScan turns deps into vulnerable_dependency findings the same way
// ScanManifestDeps would: finding_id key = repoID + advisory + package, anchored
// on manifestPath. Here every dep named "vuln-*" is treated as vulnerable.
func fakeDepsScan(_ context.Context, repoID, branch, manifestPath string, deps []ports.Dependency) ([]*domain.Finding, error) {
	var out []*domain.Finding
	for _, d := range deps {
		if !strings.HasPrefix(d.Name, "vuln-") {
			continue
		}
		f, err := domain.NewFinding(domain.FindingSpec{
			RepoID: repoID, Branch: branch,
			Severity: domain.SeverityHigh, Layer: domain.LayerSecurity,
			Rule: "vulnerable_dependency", Message: manifestPath + " " + d.Name,
		}, domain.WithFileAnchor(manifestPath), domain.WithFindingKey(repoID+"\x00"+"ADV-"+d.Name))
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, nil
}

func depScanErr(_ context.Context, _, _, _ string, _ []ports.Dependency) ([]*domain.Finding, error) {
	return nil, errors.New("advisory cache missing")
}

// goModReader parses one "name version" per line into a Go dependency. Blank
// lines ignored — so the SAME deps survive line shifts/reorders.
func goModReader(content []byte) ([]ports.Dependency, error) {
	var deps []ports.Dependency
	for line := range strings.SplitSeq(string(content), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		ver := ""
		if len(fields) > 1 {
			ver = fields[1]
		}
		deps = append(deps, ports.Dependency{Ecosystem: "Go", Name: fields[0], Version: ver})
	}
	return deps, nil
}

// refContent serves manifest bytes per (path, ref); a missing key => absent.
func refContent(byRef map[string]string) diffgate.RefContentFn {
	return func(_ context.Context, path, ref string) ([]byte, bool, error) {
		c, ok := byRef[ref+"|"+path]
		if !ok {
			return nil, false, nil
		}
		return []byte(c), true, nil
	}
}

func goModReaders() map[string]diffgate.ManifestReaderFn {
	return map[string]diffgate.ManifestReaderFn{"go.mod": goModReader}
}

func secInput(repoID string, added map[string][]checks.Line, refs map[string]string) diffgate.SecurityInput {
	return diffgate.SecurityInput{
		RepoID: repoID, Branch: "main", BaseRef: "base", CandRef: "cand",
		AddedLines: added, ReadAtRef: refContent(refs),
	}
}

func line(text string) []checks.Line { return []checks.Line{{Number: 1, Text: text}} }

// tests

// AC1: a net-new secret on an added line FAILs.
func TestSecurityGate_NewSecret_Fails(t *testing.T) {
	g := diffgate.NewSecurityGate(fakeSecrets, fakeDepsScan, goModReaders(), true)
	v, err := g.Evaluate(context.Background(), secInput("r",
		map[string][]checks.Line{"app.go": line("token := \"SECRET-abc\"")}, nil))
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if v.Pass || len(v.NewSecretLeaks) != 1 {
		t.Fatalf("want FAIL with 1 secret; got %+v", v)
	}
	if got := v.Failures(); len(got) != 1 || got[0] != diffgate.FailNewSecretLeak {
		t.Fatalf("failures = %v", got)
	}
}

// AC3: a pre-existing secret on an UNCHANGED line passes (it is never in the
// added-lines set), even with an unrelated added line.
func TestSecurityGate_PreexistingSecretUnchanged_Passes(t *testing.T) {
	g := diffgate.NewSecurityGate(fakeSecrets, fakeDepsScan, goModReaders(), true)
	// The added line carries no secret; the pre-existing secret is not scanned.
	v, err := g.Evaluate(context.Background(), secInput("r",
		map[string][]checks.Line{"app.go": line("x := 1 // unrelated")}, nil))
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if !v.Pass {
		t.Fatalf("want PASS; got %+v", v)
	}
}

// AC2: a net-new vulnerable dependency FAILs.
func TestSecurityGate_NewVuln_Fails(t *testing.T) {
	g := diffgate.NewSecurityGate(fakeSecrets, fakeDepsScan, goModReaders(), true)
	refs := map[string]string{
		"base|go.mod": "safe-a v1",
		"cand|go.mod": "safe-a v1\nvuln-b v2", // adds a vulnerable dep
	}
	v, err := g.Evaluate(context.Background(), secInput("r", nil, refs))
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if v.Pass || len(v.NewVulnDeps) != 1 {
		t.Fatalf("want FAIL with 1 new vuln; got %+v", v)
	}
	if got := v.Failures(); len(got) != 1 || got[0] != diffgate.FailNewVulnDep {
		t.Fatalf("failures = %v", got)
	}
}

// Line-move: a vulnerable dep that merely shifts line between base and candidate
// is NOT net-new — net-new is by finding_id, which excludes the line.
func TestSecurityGate_VulnLineMove_NotNetNew(t *testing.T) {
	g := diffgate.NewSecurityGate(fakeSecrets, fakeDepsScan, goModReaders(), true)
	refs := map[string]string{
		"base|go.mod": "vuln-b v2\nsafe-a v1",
		"cand|go.mod": "safe-a v1\nsafe-c v3\nvuln-b v2", // vuln-b moved + reordered
	}
	v, err := g.Evaluate(context.Background(), secInput("r", nil, refs))
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if !v.Pass {
		t.Fatalf("a moved (pre-existing) vuln must NOT be net-new; got %+v", v)
	}
}

// Base manifest absent (PR adds go.mod): base deps empty -> every candidate vuln
// is net-new -> FAIL.
func TestSecurityGate_BaseManifestAbsent_AllNew(t *testing.T) {
	g := diffgate.NewSecurityGate(fakeSecrets, fakeDepsScan, goModReaders(), true)
	refs := map[string]string{
		// no base|go.mod key -> absent at base
		"cand|go.mod": "vuln-b v2",
	}
	v, err := g.Evaluate(context.Background(), secInput("r", nil, refs))
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if v.Pass || len(v.NewVulnDeps) != 1 {
		t.Fatalf("added go.mod with a vuln must FAIL; got %+v", v)
	}
}

// Vuln not configured: dimension is not-applicable; a clean-secrets diff passes
// even though go.mod content (were it scanned) carries a vuln. No config coupling.
func TestSecurityGate_VulnNotConfigured_SecretsOnly(t *testing.T) {
	g := diffgate.NewSecurityGate(fakeSecrets, fakeDepsScan, goModReaders(), false /* vuln disabled*/)
	refs := map[string]string{"cand|go.mod": "vuln-b v2"}
	v, err := g.Evaluate(context.Background(), secInput("r", nil, refs))
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if !v.Pass || v.VulnApplicable {
		t.Fatalf("vuln-disabled clean diff must PASS with VulnApplicable=false; got %+v", v)
	}
}

// Vuln configured but the scan errors -> degraded FAIL (vuln_unchecked), never a
// false green.
func TestSecurityGate_VulnScanError_Unchecked(t *testing.T) {
	g := diffgate.NewSecurityGate(fakeSecrets, depScanErr, goModReaders(), true)
	refs := map[string]string{"base|go.mod": "safe-a v1", "cand|go.mod": "safe-a v1"}
	v, err := g.Evaluate(context.Background(), secInput("r", nil, refs))
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if v.Pass || v.VulnChecked {
		t.Fatalf("scan error must be degraded FAIL; got %+v", v)
	}
	if got := v.Failures(); len(got) != 1 || got[0] != diffgate.FailVulnUnchecked {
		t.Fatalf("failures = %v", got)
	}
}

// Clean diff (no secret, no new vuln) PASSES.
func TestSecurityGate_Clean_Passes(t *testing.T) {
	g := diffgate.NewSecurityGate(fakeSecrets, fakeDepsScan, goModReaders(), true)
	refs := map[string]string{"base|go.mod": "vuln-b v2", "cand|go.mod": "vuln-b v2\nsafe-c v3"}
	v, err := g.Evaluate(context.Background(), secInput("r",
		map[string][]checks.Line{"app.go": line("x := 1")}, refs))
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if !v.Pass || v.ExitCode() != 0 {
		t.Fatalf("clean diff must PASS; got %+v exit=%d", v, v.ExitCode())
	}
}
