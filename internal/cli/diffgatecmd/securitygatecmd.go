// SPDX-License-Identifier: AGPL-3.0-only

package diffgatecmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/whiskeyjimbo/veska/internal/application/checks"
	"github.com/whiskeyjimbo/veska/internal/application/diffgate"
	"github.com/whiskeyjimbo/veska/internal/application/manifest"
	"github.com/whiskeyjimbo/veska/internal/composition"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
	git "github.com/whiskeyjimbo/veska/internal/infrastructure/git"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/secretsscanner"
	"github.com/whiskeyjimbo/veska/internal/platform/config"
)

// SecurityParams are the net-new security gate inputs. Like the clone gate this
// is a BLANKET gate - no target finding. Unlike the other gates it needs NO
// indexed graph (no repoIndexed precondition, no sqlite): it is pure git refs +
// scanners, so it runs on a repo that was never indexed.
type SecurityParams struct {
	RepoID       string
	Branch       string
	RepoRoot     string
	BaseRef      string
	CandidateRef string
	Out          io.Writer
}

// securityReport is the JSON envelope: the verdict plus its failing-check names.
type securityReport struct {
	diffgate.SecurityVerdict
	Failures []string `json:"failures"`
}

// RunSecurity computes net-new secret_leak + vulnerable_dependency findings over
// the candidate change and emits the JSON verdict, returning ErrGateFailed on
// FAIL (the cobra layer turns that into a non-zero process exit).
func RunSecurity(ctx context.Context, p SecurityParams) error {
	if p.RepoID == "" || p.BaseRef == "" || p.CandidateRef == "" {
		return fmt.Errorf("diff-gate security: --repo, --base-ref and --candidate-ref are required")
	}
	// Security scans the diff's added lines, not the branch-scoped graph, so it
	// has no branch footgun; the (label-only) branch defaults to "main" when the
	// flag is left empty - keeping the emitted finding's branch non-empty without
	// needing the registry db this gate deliberately avoids.
	if p.Branch == "" {
		p.Branch = "main"
	}

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("diff-gate security: load config: %w", err)
	}

	// secret_leak: scan the candidate's added lines (language-agnostic).
	secretsCheck := checks.NewSecretsScanCheck(secretsscanner.New())

	// vulnerable_dependency: scan recognized manifests against the advisory
	// source. vulnEnabled mirrors the daemon - when no source is configured the
	// gate skips the dimension entirely rather than failing closed.
	vulnSource, vulnEnabled := composition.BuildVulnSource(cfg)
	// Cache-readiness fail-safe: osv.Adapter.Scan returns
	// (nil, nil) on a missing/empty advisory cache - no error. vulnEnabled
	// reflects CONFIG, not cache STATE, so a CI runner with vuln_source=osv but
	// an unrefreshed cache would otherwise silently PASS a vulnerable-dep PR.
	// When deps exist but the cache is empty we surface a scan error, which the
	// gate turns into a degraded FAIL (vuln_unchecked) rather than a false green.
	cacheReady := vulnEnabled && osvCachePopulated(config.DefaultOSVCacheDir())
	scanDeps := func(ctx context.Context, repoID, branch, manifestPath string, deps []ports.Dependency) ([]*domain.Finding, error) {
		if len(deps) == 0 {
			return nil, nil // nothing to scan - cache state is moot
		}
		if !cacheReady {
			return nil, fmt.Errorf("vuln advisory cache empty/absent at %s; refresh it before gating (vuln_source=osv is configured but no advisories are cached)", config.DefaultOSVCacheDir())
		}
		return checks.ScanManifestDeps(ctx, vulnSource, repoID, branch, manifestPath, deps, nil)
	}
	// Manifest-reader registry - the multi-language seam. go.mod is the only
	// entry until a multi-ecosystem advisory source lands (tracked separately);
	// another ecosystem's reader drops in here with no gate change.
	readers := map[string]diffgate.ManifestReaderFn{
		"go.mod": manifest.ReadGoMod,
	}

	gate := diffgate.NewSecurityGate(secretsCheck.Run, scanDeps, readers, vulnEnabled)

	addedRaw, err := git.AddedLinesBetween(ctx, p.RepoRoot, p.BaseRef, p.CandidateRef)
	if err != nil {
		return fmt.Errorf("diff-gate security: added lines: %w", err)
	}
	added := make(map[string][]checks.Line, len(addedRaw))
	for path, lines := range addedRaw {
		conv := make([]checks.Line, len(lines))
		for i, l := range lines {
			conv[i] = checks.Line{Number: l.Number, Text: l.Text}
		}
		added[path] = conv
	}

	readAtRef := func(ctx context.Context, path, ref string) ([]byte, bool, error) {
		b, err := git.FileAtRef(ctx, p.RepoRoot, ref, path)
		if err != nil {
			if errors.Is(err, git.ErrFileNotAtRef) {
				return nil, false, nil // absent at this ref (e.g. an added manifest)
			}
			return nil, false, err
		}
		return b, true, nil
	}

	verdict, err := gate.Evaluate(ctx, diffgate.SecurityInput{
		RepoID:     p.RepoID,
		Branch:     p.Branch,
		BaseRef:    p.BaseRef,
		CandRef:    p.CandidateRef,
		AddedLines: added,
		ReadAtRef:  readAtRef,
	})
	if err != nil {
		return fmt.Errorf("diff-gate security: evaluate: %w", err)
	}

	rep := securityReport{SecurityVerdict: verdict, Failures: verdict.Failures()}
	if err := emitSecurityReport(p.Out, rep); err != nil {
		return err
	}
	if !verdict.Pass {
		return fmt.Errorf("%w (%s)", ErrGateFailed, strings.Join(verdict.Failures(), ","))
	}
	return nil
}

// osvCachePopulated reports whether the OSV advisory cache directory exists and
// holds at least one advisory file - the same directory osv.Adapter.Scan reads.
// An absent dir or a dir with no files means the cache was never refreshed, so
// a clean scan result would be vacuous (the source of the silent-false-PASS the
// caller guards against).
func osvCachePopulated(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() {
			return true
		}
	}
	return false
}

func emitSecurityReport(out io.Writer, rep securityReport) error {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	if err := enc.Encode(rep); err != nil {
		return fmt.Errorf("diff-gate security: encode verdict: %w", err)
	}
	return nil
}
