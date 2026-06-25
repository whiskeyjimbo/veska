// SPDX-License-Identifier: AGPL-3.0-only

package sqlite

import (
	"testing"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
)

func TestNormalizeSignature_NameIndependentAndArity(t *testing.T) {
	// Parameter/result names must not affect the normalized form.
	a, aArity, aOK, _ := normalizeSignature("Read(p []byte) (int, error)")
	b, bArity, bOK, _ := normalizeSignature("Read(buf []byte) (n int, err error)")
	if !aOK || !bOK {
		t.Fatalf("expected both to parse: %v %v", aOK, bOK)
	}
	if a != b {
		t.Errorf("normalized forms differ:\n a=%q\n b=%q", a, b)
	}
	if aArity != bArity || aArity != "1|2" {
		t.Errorf("arity: a=%q b=%q want 1|2", aArity, bArity)
	}
}

func TestNormalizeSignature_QualifiedReduced(t *testing.T) {
	norm, _, ok, qualified := normalizeSignature("Write(w io.Writer) error")
	if !ok {
		t.Fatal("expected parse ok")
	}
	if !qualified {
		t.Error("expected qualified=true for io.Writer")
	}
	// io.Writer reduces to Writer, matching a same-package Writer reference.
	plain, _, _, _ := normalizeSignature("Write(w Writer) error")
	if norm != plain {
		t.Errorf("qualified reduction mismatch:\n q=%q\n p=%q", norm, plain)
	}
}

func TestNormalizeSignature_Unparseable(t *testing.T) {
	_, arity, ok, _ := normalizeSignature("NoParens")
	if ok {
		t.Error("expected exact=false for a signature with no params")
	}
	if arity != "0|0" {
		t.Errorf("arity: got %q want 0|0", arity)
	}
}

func TestReceiverIsPointer(t *testing.T) {
	if !receiverIsPointer("func (s *Server) Read(p []byte) (int, error) { return 0, nil }") {
		t.Error("pointer receiver not detected")
	}
	if receiverIsPointer("func (s Server) Read(p []byte) (int, error) { return 0, nil }") {
		t.Error("value receiver wrongly detected as pointer")
	}
}

func TestIsGenericDecl(t *testing.T) {
	if !isGenericDecl("type List[T any] struct { items []T }", "List") {
		t.Error("generic type not detected")
	}
	if isGenericDecl("type Server struct { name string }", "Server") {
		t.Error("non-generic type wrongly flagged generic")
	}
}

func TestSatisfies(t *testing.T) {
	read := func(ptr bool) methodSig {
		ms := parseMethodSig("Read", "Read(p []byte) (int, error)", "")
		ms.pointer = ptr
		return ms
	}
	required := []methodSig{parseMethodSig("Read", "Read(p []byte) (int, error)", "")}

	// Value type with matching method satisfies.
	have := map[string][]methodSig{"Read": {read(false)}}
	if ok, conf := satisfies(required, have); !ok || conf != domain.Definite {
		t.Errorf("value impl: ok=%v conf=%v want true/Definite", ok, conf)
	}

	// Wrong signature (different param type) does NOT satisfy.
	wrong := map[string][]methodSig{"Read": {parseMethodSig("Read", "Read(p string) (int, error)", "")}}
	if ok, _ := satisfies(required, wrong); ok {
		t.Error("near-miss (wrong signature) must not satisfy")
	}

	// Missing the method does not satisfy.
	if ok, _ := satisfies(required, map[string][]methodSig{}); ok {
		t.Error("missing method must not satisfy")
	}

	// Qualified type reduces confidence to Strong.
	reqQ := []methodSig{parseMethodSig("Write", "Write(w io.Writer) error", "")}
	haveQ := map[string][]methodSig{"Write": {parseMethodSig("Write", "Write(w Writer) error", "")}}
	if ok, conf := satisfies(reqQ, haveQ); !ok || conf != domain.Strong {
		t.Errorf("qualified match: ok=%v conf=%v want true/Strong", ok, conf)
	}
}
