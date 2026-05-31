package composition

import (
	"regexp"
	"strings"
	"testing"
)

var sha256Re = regexp.MustCompile(`^[0-9a-f]{64}$`)

// TestPotionCode16MSpec_PinnedAndVerifiable guards the install manifest:
// the base URL must be pinned to a revision (not the moving `main` ref)
// and every file must carry a syntactically valid sha256, so the
// download is reproducible and integrity-checked.
func TestPotionCode16MSpec_PinnedAndVerifiable(t *testing.T) {
	spec := PotionCode16MSpec()

	if strings.Contains(spec.BaseURL, "/resolve/main") || strings.HasSuffix(spec.BaseURL, "/main") {
		t.Errorf("BaseURL must pin a commit revision, not main: %q", spec.BaseURL)
	}
	if !strings.HasPrefix(spec.BaseURL, "https://") {
		t.Errorf("BaseURL must be https: %q", spec.BaseURL)
	}

	want := map[string]bool{"tokenizer.json": false, "model.safetensors": false}
	for _, f := range spec.Files {
		if _, ok := want[f.Name]; !ok {
			t.Errorf("unexpected file in spec: %q", f.Name)
			continue
		}
		want[f.Name] = true
		if !sha256Re.MatchString(f.SHA256) {
			t.Errorf("%s: sha256 not a 64-char hex string: %q", f.Name, f.SHA256)
		}
	}
	for name, seen := range want {
		if !seen {
			t.Errorf("spec missing required file %q", name)
		}
	}
}
