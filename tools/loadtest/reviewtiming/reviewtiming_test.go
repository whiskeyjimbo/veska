//go:build eval

// Package reviewtiming drives the M5 exit-gate-5 per-commit review time budget.
// Goal: measure the wall-clock time to run ONE review pass over a synthetic
// ~100-file commit, using the real review Handler (WorkKindReview lane) wired
// to a real local Ollama generator. The result is a MEASUREMENT, not a
// pass/fail gate - the reference-laptop number is filled into
// M5.md by a human running `make eval-review-timing`.
// What this measures: the total budget = sum of per-file review latency for
// 100 files, each file dispatched through every registered review prompt.
// Per-file mean is reported alongside the total.
// Build-tag-gated (`eval`); the make target is `make eval-review-timing`. The
// test skips with a clear message if Ollama is not reachable, so the harness
// is CI-safe and verifiable without a model.
package reviewtiming

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/whiskeyjimbo/veska/internal/application/review"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/llm"
)

const (
	defaultFileN     = 100
	defaultOllamaURL = "http://localhost:11434"
	defaultModel     = "llama3"

	// defaultLLMTimeout bounds a single review LLM call. The llm package
	// default is 60s, which is too tight for a structured-JSON review
	// generation on CPU Ollama - a real call routinely exceeds it. This
	// generous default keeps the measurement from dying on per-call
	// deadlines; override with REVIEW_TIMING_LLM_TIMEOUT.
	defaultLLMTimeout = 5 * time.Minute

	repoID = "review-timing-eval"
	branch = "main"
	gitSHA = "review-timing-commit"
)

// result is the JSON output payload.
type result struct {
	Model        string  `json:"model"`
	OllamaURL    string  `json:"ollama_url"`
	FileN        int     `json:"file_n"`
	TotalMS      int64   `json:"total_ms"`
	PerFileMS    float64 `json:"per_file_mean_ms"`
	FilesOK      int     `json:"files_reviewed_ok"`
	FilesFailed  int     `json:"files_failed"`
	WallClockBud string  `json:"wall_clock_budget"`
}

// nopFindings is a FindingStorage that discards every finding. The timing
// harness only cares about the wall-clock cost of the review pass; it does not
// persist or assert on the findings produced.
type nopFindings struct{}

func (nopFindings) Save(context.Context, *domain.Finding) error         { return nil }
func (nopFindings) CloseObsolete(context.Context, string, string) error { return nil }
func (nopFindings) CloseSupersededByRule(context.Context, string, string, string, []string) error {
	return nil
}
func (nopFindings) CloseSupersededAutoLinks(context.Context, string, string, []string) error {
	return nil
}

// TestReviewTiming is the M5 exit-gate-5 per-commit review time-budget harness.
func TestReviewTiming(t *testing.T) {
	fileN := envInt("REVIEW_TIMING_FILE_N", defaultFileN)
	ollamaURL := envStr("VESKA_OLLAMA_URL", defaultOllamaURL)
	model := envStr("VESKA_REVIEW_MODEL", defaultModel)

	if fileN <= 0 {
		t.Fatalf("REVIEW_TIMING_FILE_N must be > 0, got %d", fileN)
	}

	probeCtx, probeCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer probeCancel()
	if err := probeOllama(probeCtx, ollamaURL, model); err != nil {
		t.Skipf("Ollama review model %q not available at %s (%v) - skipping M5 exit-gate-5 review-timing harness", model, ollamaURL, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	// Build the synthetic ~100-file commit fixture: simple, distinct Go source
	// files under a temp dir that doubles as the repo root.
	root := t.TempDir()
	files := buildFixture(t, root, fileN)

	loader, err := review.NewLoader()
	if err != nil {
		t.Fatalf("review.NewLoader: %v", err)
	}
	llmTimeout := envDuration("REVIEW_TIMING_LLM_TIMEOUT", defaultLLMTimeout)
	gen := llm.NewOllamaGenerator(model, llm.WithBaseURL(ollamaURL), llm.WithTimeout(llmTimeout))
	repoRoot := func(context.Context, string) (string, error) { return root, nil }

	handler, err := review.NewHandler(gen, loader, repoRoot, nopFindings{})
	if err != nil {
		t.Fatalf("review.NewHandler: %v", err)
	}

	var filesOK, filesFailed int
	started := time.Now()
	for _, rel := range files {
		row := ports.WorkRow{
			RepoID:  repoID,
			Branch:  branch,
			GitSHA:  gitSHA,
			Kind:    ports.WorkKindReview,
			Payload: rel,
		}
		if err := handler.Handle(ctx, row); err != nil {
			filesFailed++
			t.Logf("review of %q failed: %v", rel, err)
			continue
		}
		filesOK++
	}
	total := time.Since(started)

	perFile := float64(total.Milliseconds()) / float64(fileN)
	out := result{
		Model:        model,
		OllamaURL:    ollamaURL,
		FileN:        fileN,
		TotalMS:      total.Milliseconds(),
		PerFileMS:    perFile,
		FilesOK:      filesOK,
		FilesFailed:  filesFailed,
		WallClockBud: total.String(),
	}
	emitJSON(t, out)

	// This is a measurement, not a tight gate: assert only that the run
	// completed and produced a positive duration.
	if total <= 0 {
		t.Fatalf("review pass produced a non-positive duration: %s", total)
	}
	if filesOK == 0 {
		t.Fatalf("no files were reviewed successfully (%d failed) - Ollama or model misconfigured", filesFailed)
	}
}

// buildFixture writes fileN simple Go source files under root and returns their
// repo-root-relative paths (the WorkRow.Payload values the Handler reads).
func buildFixture(t *testing.T, root string, fileN int) []string {
	t.Helper()
	dir := filepath.Join(root, "synth")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("buildFixture: mkdir: %v", err)
	}
	files := make([]string, 0, fileN)
	for i := 0; i < fileN; i++ {
		rel := filepath.Join("synth", fmt.Sprintf("file%03d.go", i))
		src := fmt.Sprintf(`package synth

// Widget%03d models a small unit of synthetic work.
type Widget%03d struct {
	ID    int
	Name  string
	Score float64
}

// Process%03d folds the widget's score into a running total and returns it.
func Process%03d(w Widget%03d, total float64) float64 {
	if w.Score < 0 {
		return total
	}
	return total + w.Score*float64(w.ID)
}
`, i, i, i, i, i)
		if err := os.WriteFile(filepath.Join(root, rel), []byte(src), 0o644); err != nil {
			t.Fatalf("buildFixture: write %q: %v", rel, err)
		}
		files = append(files, rel)
	}
	return files
}

// probeOllama issues a quick GET /api/tags and confirms the named review model
// is present. Any non-2xx response, transport failure, or missing model is
// reported as an error so the caller can t.Skip cleanly - the harness is a
// measurement, so it must not fail merely because the model is not pulled.
func probeOllama(ctx context.Context, baseURL, model string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/api/tags", nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	var tags struct {
		Models []struct {
			Name  string `json:"name"`
			Model string `json:"model"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tags); err != nil {
		return fmt.Errorf("decode /api/tags: %w", err)
	}
	for _, m := range tags.Models {
		// Match either the exact tag or the bare model name (Ollama reports
		// "<model>:latest" for an untagged pull).
		if m.Name == model || m.Model == model ||
			m.Name == model+":latest" || m.Model == model+":latest" {
			return nil
		}
	}
	return fmt.Errorf("model %q not in /api/tags (run `ollama pull %s`)", model, model)
}

func emitJSON(t *testing.T, r result) {
	t.Helper()
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	fmt.Printf("REVIEW_TIMING %s\n", string(b))
	t.Log(string(b))
}

func envInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return def
	}
	return n
}

func envStr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return def
	}
	return d
}
