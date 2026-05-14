package application

import (
	"context"
	"database/sql"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

// TestPromote_WritesSignatureAndNilPrevOnFirstPromotion verifies that the very
// first promotion of a file sets signature from the parsed Node and leaves
// prev_signature NULL — there is no prior row to carry forward, so the
// contract-drift check cannot fire by construction.
func TestPromote_WritesSignatureAndNilPrevOnFirstPromotion(t *testing.T) {
	db := openMemDB(t)
	insertTestRepo(t, db, "repo1")

	sa := NewStagingArea()
	n, _ := domain.NewNode("n1", "a.go", "Foo", domain.KindFunction,
		domain.WithSignature("func Foo() error"))
	sa.StageFile("repo1", "main", "a.go", []*domain.Node{n}, nil)

	p := NewPromoter(sa, db)
	if err := p.Promote(context.Background(), "repo1", "main", "sha-1",
		domain.Actor{ID: "service:veska", Kind: domain.ActorKindSystem}); err != nil {
		t.Fatalf("Promote: %v", err)
	}

	var sig, prev sql.NullString
	if err := db.QueryRow(
		`SELECT signature, prev_signature FROM nodes WHERE node_id=? AND branch=?`,
		"n1", "main",
	).Scan(&sig, &prev); err != nil {
		t.Fatalf("query node: %v", err)
	}
	if !sig.Valid || sig.String != "func Foo() error" {
		t.Errorf("signature: want %q, got %v", "func Foo() error", sig)
	}
	if prev.Valid {
		t.Errorf("prev_signature on first promotion: want NULL, got %q", prev.String)
	}
}

// TestPromote_ThreadsPrevSignatureAcrossPromotions verifies the core
// contract-drift mechanism: a second promotion of the same node carries the
// first promotion's signature as prev_signature.
func TestPromote_ThreadsPrevSignatureAcrossPromotions(t *testing.T) {
	db := openMemDB(t)
	insertTestRepo(t, db, "repo1")

	sa := NewStagingArea()
	p := NewPromoter(sa, db)

	// Promotion #1: signature = "func Foo() error".
	n1, _ := domain.NewNode("n1", "a.go", "Foo", domain.KindFunction,
		domain.WithSignature("func Foo() error"))
	// Sibling: a non-drifting node so we exercise multi-row prev-sig threading.
	n2, _ := domain.NewNode("n2", "a.go", "Bar", domain.KindFunction,
		domain.WithSignature("func Bar()"))
	sa.StageFile("repo1", "main", "a.go", []*domain.Node{n1, n2}, nil)
	if err := p.Promote(context.Background(), "repo1", "main", "sha-1",
		domain.Actor{ID: "service:veska", Kind: domain.ActorKindSystem}); err != nil {
		t.Fatalf("Promote 1: %v", err)
	}

	// Promotion #2: n1 signature changed; n2 unchanged.
	n1b, _ := domain.NewNode("n1", "a.go", "Foo", domain.KindFunction,
		domain.WithSignature("func Foo(ctx context.Context) error"))
	n2b, _ := domain.NewNode("n2", "a.go", "Bar", domain.KindFunction,
		domain.WithSignature("func Bar()"))
	sa.StageFile("repo1", "main", "a.go", []*domain.Node{n1b, n2b}, nil)
	if err := p.Promote(context.Background(), "repo1", "main", "sha-2",
		domain.Actor{ID: "service:veska", Kind: domain.ActorKindSystem}); err != nil {
		t.Fatalf("Promote 2: %v", err)
	}

	// n1: signature should be new, prev_signature should be old.
	var sig, prev sql.NullString
	if err := db.QueryRow(
		`SELECT signature, prev_signature FROM nodes WHERE node_id=? AND branch=?`,
		"n1", "main",
	).Scan(&sig, &prev); err != nil {
		t.Fatalf("query n1: %v", err)
	}
	if !sig.Valid || sig.String != "func Foo(ctx context.Context) error" {
		t.Errorf("n1 signature: want new, got %v", sig)
	}
	if !prev.Valid || prev.String != "func Foo() error" {
		t.Errorf("n1 prev_signature: want %q, got %v", "func Foo() error", prev)
	}

	// n2: unchanged signature -> prev_signature should equal signature.
	if err := db.QueryRow(
		`SELECT signature, prev_signature FROM nodes WHERE node_id=? AND branch=?`,
		"n2", "main",
	).Scan(&sig, &prev); err != nil {
		t.Fatalf("query n2: %v", err)
	}
	if !sig.Valid || sig.String != "func Bar()" {
		t.Errorf("n2 signature: want %q, got %v", "func Bar()", sig)
	}
	if !prev.Valid || prev.String != "func Bar()" {
		t.Errorf("n2 prev_signature: want %q (unchanged), got %v", "func Bar()", prev)
	}
}

// TestPromote_NilSignatureWritesNullColumn verifies that a node with no
// parser-supplied signature writes NULL — not "" — into the column so the
// contract-drift comparison treats it as "unknown".
func TestPromote_NilSignatureWritesNullColumn(t *testing.T) {
	db := openMemDB(t)
	insertTestRepo(t, db, "repo1")

	sa := NewStagingArea()
	n, _ := domain.NewNode("n-nosig", "a.go", "Foo", domain.KindField)
	sa.StageFile("repo1", "main", "a.go", []*domain.Node{n}, nil)

	p := NewPromoter(sa, db)
	if err := p.Promote(context.Background(), "repo1", "main", "sha",
		domain.Actor{ID: "service:veska", Kind: domain.ActorKindSystem}); err != nil {
		t.Fatalf("Promote: %v", err)
	}

	var sig sql.NullString
	if err := db.QueryRow(
		`SELECT signature FROM nodes WHERE node_id=?`, "n-nosig",
	).Scan(&sig); err != nil {
		t.Fatalf("query: %v", err)
	}
	if sig.Valid {
		t.Errorf("signature: want NULL (no parser signature), got %q", sig.String)
	}
}
