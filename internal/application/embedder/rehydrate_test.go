// SPDX-License-Identifier: AGPL-3.0-only

package embedder_test

import (
	"context"
	"errors"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/application/embedder"
	"github.com/whiskeyjimbo/veska/internal/application/veccodec"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

// TestRehydrateVectors_LoadsReadyRows: the rows yielded by the loader hydrate
// the spy VectorStorage with one UpsertEmbeddings call per (repo, branch)
// bucket. Stand-in for the daemon-restart scenario that motivated.
// The state='ready' filtering is the loader's job (covered by the sqlite
// EmbeddingArchive test); here we verify bucketing + decode + upsert.
func TestRehydrateVectors_LoadsReadyRows(t *testing.T) {
	// Two buckets: repo1/main and repo1/topic, two nodes in main, one in topic.
	loader := &fakeLoader{rows: []embedder.ReadyEmbeddingRow{
		readyRow("repo1", "main", "n1", "h1", "m1", []float32{1, 0, 0}),
		readyRow("repo1", "main", "n2", "h2", "m1", []float32{0, 1, 0}),
		readyRow("repo1", "topic", "n3", "h3", "m1", []float32{0, 0, 1}),
	}}

	vec := &spyVector{}
	counts, err := embedder.RehydrateVectors(context.Background(), loader, vec)
	if err != nil {
		t.Fatalf("RehydrateVectors: %v", err)
	}
	if got := counts["repo1@main"]; got != 2 {
		t.Errorf("counts[repo1@main] = %d, want 2", got)
	}
	if got := counts["repo1@topic"]; got != 1 {
		t.Errorf("counts[repo1@topic] = %d, want 1", got)
	}
	if len(vec.calls) != 2 {
		t.Fatalf("UpsertEmbeddings calls = %d, want 2; %+v", len(vec.calls), vec.calls)
	}
}

func TestRehydrateVectors_NilDeps(t *testing.T) {
	if _, err := embedder.RehydrateVectors(context.Background(), nil, &spyVector{}); !errors.Is(err, embedder.ErrMissingDependency) {
		t.Errorf("nil loader: want ErrMissingDependency, got %v", err)
	}
	if _, err := embedder.RehydrateVectors(context.Background(), &fakeLoader{}, nil); !errors.Is(err, embedder.ErrMissingDependency) {
		t.Errorf("nil vectors: want ErrMissingDependency, got %v", err)
	}
}

// TestRehydrateVectors_LoadError surfaces a loader failure wrapped, rather than
// silently hydrating an empty store.
func TestRehydrateVectors_LoadError(t *testing.T) {
	loader := &fakeLoader{err: errors.New("boom")}
	if _, err := embedder.RehydrateVectors(context.Background(), loader, &spyVector{}); err == nil {
		t.Fatal("want error from loader, got nil")
	}
}

// TestRehydrateVectors_Idempotent: a second invocation produces the same store
// contents. The contract relies on VectorStorage.UpsertEmbeddings keying by
// node_id within (repo, branch), so multiple runs do not bloat the store - the
// spy here only counts call shape, not store state.
func TestRehydrateVectors_Idempotent(t *testing.T) {
	loader := &fakeLoader{rows: []embedder.ReadyEmbeddingRow{
		readyRow("repo1", "main", "n1", "h1", "m1", []float32{1, 0, 0}),
	}}

	vec := &spyVector{}
	if _, err := embedder.RehydrateVectors(context.Background(), loader, vec); err != nil {
		t.Fatalf("first: %v", err)
	}
	if _, err := embedder.RehydrateVectors(context.Background(), loader, vec); err != nil {
		t.Fatalf("second: %v", err)
	}
	if len(vec.calls) != 2 {
		t.Errorf("two runs → want 2 Upsert calls (one per run), got %d", len(vec.calls))
	}
	for i, c := range vec.calls {
		if len(c.batch) != 1 {
			t.Errorf("run %d batch size = %d, want 1", i, len(c.batch))
		}
	}
}

// ── test helpers ──────────────────────────────────────────────────────────────

type fakeLoader struct {
	rows []embedder.ReadyEmbeddingRow
	err  error
}

func (f *fakeLoader) LoadReadyEmbeddings(context.Context) ([]embedder.ReadyEmbeddingRow, error) {
	return f.rows, f.err
}

func readyRow(repo, branch, nodeID, contentHash, model string, vec []float32) embedder.ReadyEmbeddingRow {
	return embedder.ReadyEmbeddingRow{
		RepoID:      repo,
		Branch:      branch,
		NodeID:      nodeID,
		ContentHash: contentHash,
		ModelID:     model,
		Dim:         len(vec),
		Blob:        veccodec.EncodeFloat32LE(vec),
	}
}

type spyCall struct {
	repo, branch string
	batch        []domain.EmbeddingRow
}

type spyVector struct{ calls []spyCall }

func (s *spyVector) UpsertEmbeddings(_ context.Context, repoID, branch string, batch []domain.EmbeddingRow) error {
	cp := make([]domain.EmbeddingRow, len(batch))
	copy(cp, batch)
	s.calls = append(s.calls, spyCall{repo: repoID, branch: branch, batch: cp})
	return nil
}

func (s *spyVector) Search(_ context.Context, _, _ string, _ []float32, _ int, _ domain.VectorFilter) ([]domain.SearchHit, error) {
	return nil, nil
}

func (s *spyVector) LookupContentHashes(context.Context, string, string, []string) (map[string]string, error) {
	return nil, nil
}

func (s *spyVector) DeleteNodes(context.Context, string, string, []string) error { return nil }
func (s *spyVector) Reindex(context.Context, string, string) error               { return nil }
