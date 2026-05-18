package review_test

import (
	"errors"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/application/review"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

// update regenerates the golden rendered-prompt fixtures: go test -run X -update.
var update = flag.Bool("update", false, "regenerate golden prompt fixtures")

// fixtureInput is the deterministic code-under-review used by every
// record/replay test so the golden rendered prompts stay stable.
var fixtureInput = review.Input{
	RepoID:         "repo-1",
	Branch:         "main",
	FilePath:       "internal/svc/user.go",
	Code:           "func Fetch(id string) ([]byte, error) { return db.Query(\"select * from users where id=\" + id) }",
	PriorSignature: "func Fetch(id string) (string, error)",
}

func newLoader(t *testing.T) *review.Loader {
	t.Helper()
	l, err := review.NewLoader()
	if err != nil {
		t.Fatalf("NewLoader: %v", err)
	}
	return l
}

// AC1: the loader returns a versioned prompt for each review kind.
func TestLoader_VersionedPromptPerKind(t *testing.T) {
	l := newLoader(t)
	for _, kind := range []review.ReviewKind{review.KindSecurity, review.KindContractDrift} {
		p, err := l.LoadPrompt(kind)
		if err != nil {
			t.Fatalf("LoadPrompt(%s): %v", kind, err)
		}
		if p.Kind() != kind {
			t.Errorf("Kind() = %q, want %q", p.Kind(), kind)
		}
		if p.Version() == "" {
			t.Errorf("Version() for %s is empty", kind)
		}
	}
}

// AC1: an unknown kind is a returned error, not a panic.
func TestLoader_UnknownKindErrors(t *testing.T) {
	l := newLoader(t)
	if _, err := l.LoadPrompt(review.ReviewKind("bogus")); !errors.Is(err, review.ErrUnknownKind) {
		t.Fatalf("LoadPrompt(bogus) err = %v, want ErrUnknownKind", err)
	}
}

func TestLoader_Kinds(t *testing.T) {
	l := newLoader(t)
	got := l.Kinds()
	if len(got) != 2 {
		t.Fatalf("Kinds() = %v, want 2 kinds", got)
	}
	// sorted: contract_drift < security
	if got[0] != review.KindContractDrift || got[1] != review.KindSecurity {
		t.Errorf("Kinds() = %v, want deterministic sorted order", got)
	}
}

// AC2: record/replay — render the prompt (golden compare) then parse a
// committed model-output fixture and assert the structured findings.
func TestRecordReplay(t *testing.T) {
	cases := []struct {
		kind        review.ReviewKind
		goldenFile  string
		outputFile  string
		wantVersion string
		want        []review.ReviewFinding
	}{
		{
			kind:        review.KindSecurity,
			goldenFile:  "security.rendered.golden",
			outputFile:  "security.model_output.txt",
			wantVersion: "security.v1",
			want: []review.ReviewFinding{
				{
					Title:    "SQL injection in user lookup",
					Message:  "The query is built by string concatenation of the untrusted id parameter; use a parameterized query instead.",
					Severity: domain.SeverityHigh,
					Kind:     review.KindSecurity,
				},
				{
					Title:    "Hardcoded credential",
					Message:  "An API token is embedded as a string literal; move it to configuration or a secret store.",
					Severity: domain.SeverityMedium,
					Kind:     review.KindSecurity,
				},
			},
		},
		{
			kind:        review.KindContractDrift,
			goldenFile:  "contract_drift.rendered.golden",
			outputFile:  "contract_drift.model_output.txt",
			wantVersion: "contract_drift.v1",
			want: []review.ReviewFinding{
				{
					Title:    "Return type changed on exported Fetch",
					Message:  "Fetch previously returned (string, error) and now returns ([]byte, error); this breaks every existing caller.",
					Severity: domain.SeverityHigh,
					Kind:     review.KindContractDrift,
				},
			},
		},
	}

	l := newLoader(t)
	for _, tc := range cases {
		t.Run(string(tc.kind), func(t *testing.T) {
			p, err := l.LoadPrompt(tc.kind)
			if err != nil {
				t.Fatalf("LoadPrompt: %v", err)
			}
			if p.Version() != tc.wantVersion {
				t.Errorf("Version() = %q, want %q", p.Version(), tc.wantVersion)
			}

			rendered, err := p.Render(fixtureInput)
			if err != nil {
				t.Fatalf("Render: %v", err)
			}
			goldenPath := filepath.Join("testdata", tc.goldenFile)
			if *update {
				if err := os.WriteFile(goldenPath, []byte(rendered), 0o644); err != nil {
					t.Fatalf("update golden: %v", err)
				}
			}
			wantRendered, err := os.ReadFile(goldenPath)
			if err != nil {
				t.Fatalf("read golden: %v", err)
			}
			if rendered != string(wantRendered) {
				t.Errorf("rendered prompt mismatch:\n got %q\nwant %q", rendered, wantRendered)
			}

			// Render is deterministic: same input, byte-identical output.
			again, err := p.Render(fixtureInput)
			if err != nil {
				t.Fatalf("Render (2nd): %v", err)
			}
			if again != rendered {
				t.Error("Render is not deterministic")
			}

			output, err := os.ReadFile(filepath.Join("testdata", tc.outputFile))
			if err != nil {
				t.Fatalf("read model output fixture: %v", err)
			}
			got, err := p.Parse(string(output))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("Parse returned %d findings, want %d: %+v", len(got), len(tc.want), got)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Errorf("finding %d:\n got %+v\nwant %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestRender_EmptyCodeErrors(t *testing.T) {
	l := newLoader(t)
	p, _ := l.LoadPrompt(review.KindSecurity)
	in := fixtureInput
	in.Code = "   "
	if _, err := p.Render(in); !errors.Is(err, review.ErrEmptyInput) {
		t.Fatalf("Render(empty) err = %v, want ErrEmptyInput", err)
	}
}

func TestParse_NoFindingsSentinel(t *testing.T) {
	l := newLoader(t)
	p, _ := l.LoadPrompt(review.KindSecurity)
	got, err := p.Parse("NO FINDINGS")
	if err != nil {
		t.Fatalf("Parse(NO FINDINGS): %v", err)
	}
	if len(got) != 0 {
		t.Errorf("Parse(NO FINDINGS) = %+v, want empty", got)
	}
}

func TestParse_Malformed(t *testing.T) {
	l := newLoader(t)
	p, _ := l.LoadPrompt(review.KindSecurity)
	cases := map[string]string{
		"empty":            "",
		"missing severity": "TITLE: x\nMESSAGE: y",
		"missing title":    "SEVERITY: high\nMESSAGE: y",
		"missing message":  "SEVERITY: high\nTITLE: x",
		"invalid severity": "SEVERITY: catastrophic\nTITLE: x\nMESSAGE: y",
		"garbage":          "the model said something unstructured",
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := p.Parse(in); !errors.Is(err, review.ErrMalformedResponse) {
				t.Fatalf("Parse(%q) err = %v, want ErrMalformedResponse", name, err)
			}
		})
	}
}
