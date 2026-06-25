// SPDX-License-Identifier: AGPL-3.0-only

package sqlite_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/whiskeyjimbo/veska/internal/application"
	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/sqlite"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/treesitter"
)

// parseToFile parses Go source through the real parser into a PromotionFile so
// type-relation resolution is exercised end to end (parser -> promotion).
func parseToFile(t *testing.T, repoID, path, src string) application.PromotionFile {
	t.Helper()
	res, err := treesitter.NewGoParser().ParseFile(context.Background(), repoID, path, []byte(src))
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return application.PromotionFile{
		Path:            path,
		Nodes:           res.Nodes,
		Edges:           res.Edges,
		UnresolvedCalls: res.UnresolvedCalls,
		Imports:         res.Imports,
		TypeRels:        res.TypeRels,
	}
}

func typeResolveStore(t *testing.T) (*sqlite.PromotionStore, func(string, ...any) (int, error)) {
	t.Helper()
	db := openTest(t, filepath.Join(t.TempDir(), "v.db"))
	if _, err := db.Exec(
		`INSERT INTO repos (repo_id, root_path, added_at, module_path) VALUES (?, ?, ?, ?)`,
		"repo1", "/tmp/app", time.Now().UnixMilli(), "example.com/app",
	); err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	store := sqlite.NewPromotionStore(db, []sqlite.PromotionSink{sqlite.NewFTSSink(), sqlite.NewEmbedRefSink()})
	count := func(q string, args ...any) (int, error) {
		var n int
		err := db.QueryRow(q, args...).Scan(&n)
		return n, err
	}
	return store, count
}

// edgeCount counts edges of kind joining a source symbol to a destination symbol.
const edgeCountQuery = `
SELECT COUNT(*) FROM edges e
JOIN nodes s ON s.node_id = e.src_node_id AND s.branch = e.branch
JOIN nodes d ON d.node_id = e.dst_node_id AND d.branch = e.branch
WHERE e.repo_id = 'repo1' AND e.branch = 'main' AND e.kind = ?
  AND s.symbol_path = ? AND d.symbol_path = ?`

func TestPromotion_EmbedsEdge(t *testing.T) {
	t.Parallel()
	store, count := typeResolveStore(t)

	src := `package app

type Base struct{}

type Server struct {
	*Base
	name string
}
`
	if err := store.Promote(context.Background(), application.PromotionBatch{
		RepoID: "repo1", Branch: "main", GitSHA: "s1", Actor: systemActor(),
		PromotedAt: time.Now().UnixMilli(),
		Files:      []application.PromotionFile{parseToFile(t, "repo1", "/tmp/app/server.go", src)},
	}); err != nil {
		t.Fatalf("promote: %v", err)
	}

	n, err := count(edgeCountQuery, string(domain.EdgeEmbeds), "Server", "Base")
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("want 1 EMBEDS Server->Base, got %d", n)
	}
}

func TestPromotion_ImplementsValueAndNearMiss(t *testing.T) {
	t.Parallel()
	store, count := typeResolveStore(t)

	src := `package app

type Reader interface {
	Read(p []byte) (int, error)
}

type GoodReader struct{}

func (g GoodReader) Read(p []byte) (int, error) { return 0, nil }

type BadReader struct{}

func (b BadReader) Read(p string) (int, error) { return 0, nil }
`
	if err := store.Promote(context.Background(), application.PromotionBatch{
		RepoID: "repo1", Branch: "main", GitSHA: "s1", Actor: systemActor(),
		PromotedAt: time.Now().UnixMilli(),
		Files:      []application.PromotionFile{parseToFile(t, "repo1", "/tmp/app/reader.go", src)},
	}); err != nil {
		t.Fatalf("promote: %v", err)
	}

	good, _ := count(edgeCountQuery, string(domain.EdgeImplements), "GoodReader", "Reader")
	if good != 1 {
		t.Errorf("want GoodReader IMPLEMENTS Reader, got %d", good)
	}
	bad, _ := count(edgeCountQuery, string(domain.EdgeImplements), "BadReader", "Reader")
	if bad != 0 {
		t.Errorf("near-miss BadReader must NOT implement Reader, got %d", bad)
	}
}

func TestPromotion_ImplementsPointerReceiver(t *testing.T) {
	t.Parallel()
	store, count := typeResolveStore(t)

	src := `package app

type Reader interface {
	Read(p []byte) (int, error)
}

type PtrReader struct{}

func (r *PtrReader) Read(p []byte) (int, error) { return 0, nil }
`
	if err := store.Promote(context.Background(), application.PromotionBatch{
		RepoID: "repo1", Branch: "main", GitSHA: "s1", Actor: systemActor(),
		PromotedAt: time.Now().UnixMilli(),
		Files:      []application.PromotionFile{parseToFile(t, "repo1", "/tmp/app/ptr.go", src)},
	}); err != nil {
		t.Fatalf("promote: %v", err)
	}
	// A pointer-receiver method still means the type implements (via *PtrReader).
	n, _ := count(edgeCountQuery, string(domain.EdgeImplements), "PtrReader", "Reader")
	if n != 1 {
		t.Errorf("want PtrReader IMPLEMENTS Reader (pointer receiver), got %d", n)
	}
}

func TestPromotion_ImplementsViaEmbeddedPromotion(t *testing.T) {
	t.Parallel()
	store, count := typeResolveStore(t)

	src := `package app

type Reader interface {
	Read(p []byte) (int, error)
}

type Base struct{}

func (b Base) Read(p []byte) (int, error) { return 0, nil }

type Wrapper struct {
	Base
}
`
	if err := store.Promote(context.Background(), application.PromotionBatch{
		RepoID: "repo1", Branch: "main", GitSHA: "s1", Actor: systemActor(),
		PromotedAt: time.Now().UnixMilli(),
		Files:      []application.PromotionFile{parseToFile(t, "repo1", "/tmp/app/embed.go", src)},
	}); err != nil {
		t.Fatalf("promote: %v", err)
	}
	// Wrapper gets Read promoted from embedded Base, so it implements Reader.
	n, _ := count(edgeCountQuery, string(domain.EdgeImplements), "Wrapper", "Reader")
	if n != 1 {
		t.Errorf("want Wrapper IMPLEMENTS Reader via embedded Base, got %d", n)
	}
}

func TestPromotion_ImplementsInterfaceEmbedsInterface(t *testing.T) {
	t.Parallel()
	store, count := typeResolveStore(t)

	src := `package app

type Reader interface {
	Read(p []byte) (int, error)
}

type Writer interface {
	Write(p []byte) (int, error)
}

type ReadWriter interface {
	Reader
	Writer
}

type File struct{}

func (f File) Read(p []byte) (int, error)  { return 0, nil }
func (f File) Write(p []byte) (int, error) { return 0, nil }
`
	if err := store.Promote(context.Background(), application.PromotionBatch{
		RepoID: "repo1", Branch: "main", GitSHA: "s1", Actor: systemActor(),
		PromotedAt: time.Now().UnixMilli(),
		Files:      []application.PromotionFile{parseToFile(t, "repo1", "/tmp/app/rw.go", src)},
	}); err != nil {
		t.Fatalf("promote: %v", err)
	}
	// File satisfies ReadWriter only if the embedded Reader+Writer method sets
	// were flattened into ReadWriter's required set.
	n, _ := count(edgeCountQuery, string(domain.EdgeImplements), "File", "ReadWriter")
	if n != 1 {
		t.Errorf("want File IMPLEMENTS ReadWriter (interface embedding), got %d", n)
	}
}
