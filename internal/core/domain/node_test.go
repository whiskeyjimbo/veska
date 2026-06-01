package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func sha256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// DoD 1: NewNode with empty id returns error.
func TestNewNode_EmptyID(t *testing.T) {
	_, err := NewNode(NodeSpec{ID: "", Path: "pkg/foo.go", Name: "Foo", Kind: KindFunction})
	if err == nil {
		t.Fatal("expected error for empty id, got nil")
		return
	}
}

// NewNode with empty path returns error (previously unchecked).
func TestNewNode_EmptyPath(t *testing.T) {
	_, err := NewNode(NodeSpec{ID: "abc", Path: "", Name: "Foo", Kind: KindFunction})
	if err == nil {
		t.Fatal("expected error for empty path, got nil")
	}
}

// NewNode with empty name returns error (previously unchecked).
func TestNewNode_EmptyName(t *testing.T) {
	_, err := NewNode(NodeSpec{ID: "abc", Path: "pkg/foo.go", Name: "", Kind: KindFunction})
	if err == nil {
		t.Fatal("expected error for empty name, got nil")
	}
}

// DoD 2: NewNode with valid required fields returns non-nil Node.
func TestNewNode_ValidRequired(t *testing.T) {
	n, err := NewNode(NodeSpec{ID: "abc", Path: "pkg/foo.go", Name: "Foo", Kind: KindFunction})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n == nil {
		t.Fatal("expected non-nil Node")
		return
	}
	if n.ID != NodeID("abc") {
		t.Errorf("ID: got %q, want %q", n.ID, "abc")
	}
	if n.Path != "pkg/foo.go" {
		t.Errorf("Path: got %q, want %q", n.Path, "pkg/foo.go")
	}
	if n.Name != "Foo" {
		t.Errorf("Name: got %q, want %q", n.Name, "Foo")
	}
	if n.Kind != KindFunction {
		t.Errorf("Kind: got %q, want %q", n.Kind, KindFunction)
	}
}

// DoD 3a: WithContentHash + WithRawContent — matching hash succeeds.
func TestNewNode_ContentHashMatchesRawContent(t *testing.T) {
	raw := "func Foo() {}"
	hash := sha256Hex(raw)
	_, err := NewNode(NodeSpec{ID: "abc", Path: "pkg/foo.go", Name: "Foo", Kind: KindFunction}, WithRawContent(raw), WithContentHash(ContentHash(hash)))
	if err != nil {
		t.Fatalf("unexpected error with matching hash: %v", err)
	}
}

// DoD 3b: WithContentHash + WithRawContent — mismatched hash returns error.
func TestNewNode_ContentHashMismatch(t *testing.T) {
	_, err := NewNode(NodeSpec{ID: "abc", Path: "pkg/foo.go", Name: "Foo", Kind: KindFunction}, WithRawContent("func Foo() {}"), WithContentHash("deadbeef"))
	if err == nil {
		t.Fatal("expected error for mismatched content hash, got nil")
		return
	}
}

// DoD 4: WithContentHash alone (no raw_content) is allowed.
func TestNewNode_ContentHashAlone(t *testing.T) {
	_, err := NewNode(NodeSpec{ID: "abc", Path: "pkg/foo.go", Name: "Foo", Kind: KindFunction}, WithContentHash("aabbccdd"))
	if err != nil {
		t.Fatalf("unexpected error for hash-only option: %v", err)
	}
}

// DoD 5: WithLines with start > end returns error.
func TestNewNode_LinesStartAfterEnd(t *testing.T) {
	_, err := NewNode(NodeSpec{ID: "abc", Path: "pkg/foo.go", Name: "Foo", Kind: KindFunction}, WithLines(LineRange{Start: 10, End: 5}))
	if err == nil {
		t.Fatal("expected error for start > end in LineRange, got nil")
		return
	}
}

// WithLines with start == end is valid.
func TestNewNode_LinesStartEqualsEnd(t *testing.T) {
	_, err := NewNode(NodeSpec{ID: "abc", Path: "pkg/foo.go", Name: "Foo", Kind: KindFunction}, WithLines(LineRange{Start: 5, End: 5}))
	if err != nil {
		t.Fatalf("unexpected error for start == end: %v", err)
	}
}

// WithLines with start < 1 (0-indexed) returns error.
func TestNewNode_LinesZeroIndexed(t *testing.T) {
	_, err := NewNode(NodeSpec{ID: "abc", Path: "pkg/foo.go", Name: "Foo", Kind: KindFunction}, WithLines(LineRange{Start: 0, End: 5}))
	if err == nil {
		t.Fatal("expected error for 0-indexed start, got nil")
		return
	}
}

// NodeKind enum round-trip.
func TestNodeKindValues(t *testing.T) {
	kinds := []NodeKind{
		KindFunction, KindMethod, KindType, KindStruct,
		KindInterface, KindClass, KindModule, KindPackage,
		KindFile, KindField, KindTest,
	}
	if len(kinds) != 11 {
		t.Errorf("expected 11 NodeKind values, got %d", len(kinds))
	}
}

// NewNode accepts every valid NodeKind and rejects an unknown kind.
func TestNewNode_KindValidation(t *testing.T) {
	valid := []NodeKind{
		KindFunction, KindMethod, KindType, KindStruct,
		KindInterface, KindClass, KindModule, KindPackage,
		KindFile, KindField, KindTest, KindVariable, KindChunk,
	}
	if len(valid) != 13 {
		t.Fatalf("expected 13 valid NodeKinds, listed %d", len(valid))
	}
	for _, k := range valid {
		t.Run(string(k), func(t *testing.T) {
			_, err := NewNode(NodeSpec{ID: "abc", Path: "pkg/foo.go", Name: "Foo", Kind: k})
			if err != nil {
				t.Fatalf("valid kind %q rejected: %v", k, err)
			}
		})
	}
	if _, err := NewNode(NodeSpec{ID: "abc", Path: "pkg/foo.go", Name: "Foo", Kind: NodeKind("bogus")}); err == nil {
		t.Fatal("expected error for unknown NodeKind, got nil")
	}
}

// Optional fields are nil by default.
func TestNewNode_OptionalFieldsNilByDefault(t *testing.T) {
	n, err := NewNode(NodeSpec{ID: "abc", Path: "pkg/foo.go", Name: "Foo", Kind: KindFunction})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n.Signature != nil {
		t.Error("Signature should be nil by default")
	}
	if n.Lines != nil {
		t.Error("Lines should be nil by default")
	}
	if n.RawContent != nil {
		t.Error("RawContent should be nil by default")
	}
	if n.ContentHash != nil {
		t.Error("ContentHash should be nil by default")
	}
	if n.Language != nil {
		t.Error("Language should be nil by default")
	}
	if n.Exported != nil {
		t.Error("Exported should be nil by default")
	}
}

// WithSignature sets the signature.
func TestNewNode_WithSignature(t *testing.T) {
	sig := "func Foo() error"
	n, err := NewNode(NodeSpec{ID: "abc", Path: "pkg/foo.go", Name: "Foo", Kind: KindFunction}, WithSignature(sig))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n.Signature == nil || *n.Signature != sig {
		t.Errorf("Signature: got %v, want %q", n.Signature, sig)
	}
}

// WithLanguage sets the language.
func TestNewNode_WithLanguage(t *testing.T) {
	n, err := NewNode(NodeSpec{ID: "abc", Path: "pkg/foo.go", Name: "Foo", Kind: KindFunction}, WithLanguage("go"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n.Language == nil || *n.Language != "go" {
		t.Errorf("Language: got %v, want %q", n.Language, "go")
	}
}

// WithExported sets exported flag.
func TestNewNode_WithExported(t *testing.T) {
	n, err := NewNode(NodeSpec{ID: "abc", Path: "pkg/foo.go", Name: "Foo", Kind: KindFunction}, WithExported(true))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n.Exported == nil || *n.Exported != true {
		t.Errorf("Exported: got %v, want true", n.Exported)
	}
}
