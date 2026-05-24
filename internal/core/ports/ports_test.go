// Package ports_test contains compile-time interface surface tests.
// These blank-identifier checks ensure that all stub implementations satisfy
// every interface in the ports package. The stubs have empty method bodies;
// their only purpose is to make the compiler validate the full method set.
//
// Adding a new method to an interface will cause a compile error here until
// the stub (and any real adapter) is updated — giving you fast, zero-overhead
// coverage of the interface contract.
package ports_test

import (
	"context"
	"time"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/core/ports"
)

// Compile-time interface compliance checks.
var (
	_ ports.GraphStorage      = (*stubGraphStorage)(nil)
	_ ports.CodeParser        = (*stubCodeParser)(nil)
	_ ports.EmbeddingProvider = (*stubEmbeddingProvider)(nil)
	_ ports.Watcher           = (*stubWatcher)(nil)
	_ ports.AuditWriter       = (*stubAuditWriter)(nil)
	_ ports.VectorStorage     = (*stubVectorStorage)(nil)
	_ ports.Tracker           = (*stubTracker)(nil)
	_ ports.VulnSource        = (*stubVulnSource)(nil)
	_ ports.Embedder          = (*stubEmbedder)(nil)
	_ ports.LLMGenerator      = (*stubLLMGenerator)(nil)
	_ ports.Notifier          = (*stubNotifier)(nil)
	_ ports.SecretsScanner    = (*stubSecretsScanner)(nil)
)

// ── GraphStorage stub ──────────────────────────────────────────────────────

type stubGraphStorage struct{}

func (s *stubGraphStorage) SaveNode(_ context.Context, _, _ string, _ *domain.Node) error {
	return nil
}
func (s *stubGraphStorage) SaveEdge(_ context.Context, _, _ string, _ *domain.Edge) error {
	return nil
}
func (s *stubGraphStorage) DeleteFile(_ context.Context, _, _, _ string) error { return nil }
func (s *stubGraphStorage) LoadGraph(_ context.Context, _, _ string) (*domain.Graph, error) {
	return nil, nil
}
func (s *stubGraphStorage) FindNodes(_ context.Context, _, _, _ string) ([]*domain.Node, error) {
	return nil, nil
}
func (s *stubGraphStorage) NodesForFile(_ context.Context, _, _, _ string) ([]*domain.Node, error) {
	return nil, nil
}
func (s *stubGraphStorage) GetNode(_ context.Context, _, _ string, _ domain.NodeID) (*domain.Node, error) {
	return nil, nil
}
func (s *stubGraphStorage) FindNodeByID(_ context.Context, _ domain.NodeID) (*domain.Node, error) {
	return nil, nil
}

// ── CodeParser stub ────────────────────────────────────────────────────────

type stubCodeParser struct{}

func (s *stubCodeParser) ParseFile(_ context.Context, _, _ string, _ []byte) (*domain.ParseResult, error) {
	return nil, nil
}

// ── EmbeddingProvider stub ─────────────────────────────────────────────────

type stubEmbeddingProvider struct{}

func (s *stubEmbeddingProvider) Embed(_ context.Context, _ string) ([]float32, error) {
	return nil, nil
}
func (s *stubEmbeddingProvider) ModelID() string { return "" }

// ── Watcher stub ───────────────────────────────────────────────────────────

type stubWatcher struct{}

func (s *stubWatcher) Watch(_ context.Context, _ string) (<-chan ports.FileEvent, error) {
	return nil, nil
}
func (s *stubWatcher) Close() error { return nil }

// ── AuditWriter stub ───────────────────────────────────────────────────────

type stubAuditWriter struct{}

func (s *stubAuditWriter) Write(_ context.Context, _ ports.AuditEntry) error { return nil }

// ── VectorStorage stub ─────────────────────────────────────────────────────

type stubVectorStorage struct{}

func (s *stubVectorStorage) UpsertEmbeddings(_ context.Context, _, _ string, _ []domain.EmbeddingRow) error {
	return nil
}
func (s *stubVectorStorage) Search(_ context.Context, _, _ string, _ []float32, _ int, _ domain.Filter) ([]domain.Hit, error) {
	return nil, nil
}
func (s *stubVectorStorage) Reindex(_ context.Context, _, _ string) error { return nil }
func (s *stubVectorStorage) LookupContentHashes(_ context.Context, _, _ string, _ []string) (map[string]string, error) {
	return nil, nil
}

// ── Tracker stub ──────────────────────────────────────────────────────────

type stubTracker struct{}

func (s *stubTracker) ActiveTask(_ context.Context, _ string) (*ports.Task, error) { return nil, nil }
func (s *stubTracker) RecentTasks(_ context.Context, _ string, _ int) ([]ports.Task, error) {
	return nil, nil
}

// ── VulnSource stub ────────────────────────────────────────────────────────

type stubVulnSource struct{}

func (s *stubVulnSource) Refresh(_ context.Context) error { return nil }

func (s *stubVulnSource) Scan(_ context.Context, _ []ports.Dependency) ([]ports.VulnFinding, error) {
	return nil, nil
}

// ── Embedder stub ──────────────────────────────────────────────────────────
// Embedder is an alias for EmbeddingProvider; reuse stubEmbeddingProvider.
type stubEmbedder = stubEmbeddingProvider

// ── LLMGenerator stub ─────────────────────────────────────────────────────

type stubLLMGenerator struct{}

func (s *stubLLMGenerator) Generate(_ context.Context, _ ports.GenerateRequest) (ports.GenerateResponse, error) {
	return ports.GenerateResponse{}, nil
}

// ── Notifier stub ──────────────────────────────────────────────────────────

type stubNotifier struct{}

func (s *stubNotifier) Notify(_ context.Context, _ ports.Notification) error { return nil }

// ── SecretsScanner stub ────────────────────────────────────────────────────

type stubSecretsScanner struct{}

func (s *stubSecretsScanner) Scan(_ ports.ScanInput) ([]ports.SecretFinding, error) {
	return nil, nil
}

// Prevent "imported and not used" errors for packages only referenced by stubs.
var _ = time.Time{}
