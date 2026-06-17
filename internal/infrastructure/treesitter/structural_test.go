package treesitter_test

import (
	"context"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/infrastructure/treesitter"
)

// TestStructuralHash_Type2Clones verifies that consistent renaming of symbols and
// variables leaves the structural hash unchanged (identifying Type-2 clones),
// while logic changes modify it.
func TestStructuralHash_Type2Clones(t *testing.T) {
	src := []byte(`package p

func Sum(a, b int) int {
	total := a + b
	return total
}

func SumCopy(a, b int) int {
	total := a + b
	return total
}

func Add(x, y int) int {
	result := x + y
	return result
}

func Mul(a, b int) int {
	return a * b
}
`)
	p := treesitter.NewGoParser()
	res, err := p.ParseFile(context.Background(), repoID, filePath, src)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}

	sh := func(name string) (structural, content string) {
		n := findNodeByName(res.Nodes, name)
		if n == nil {
			t.Fatalf("node %q not found", name)
		}
		if n.StructuralHash == nil {
			t.Fatalf("node %q has no structural_hash", name)
		}
		if n.ContentHash == nil {
			t.Fatalf("node %q has no content_hash", name)
		}
		return string(*n.StructuralHash), string(*n.ContentHash)
	}

	sumS, sumC := sh("Sum")
	copyS, copyC := sh("SumCopy")
	addS, addC := sh("Add")
	mulS, _ := sh("Mul")

	// A name-only rename or consistently renamed body both yield matching structural hashes.
	if sumS != copyS {
		t.Errorf("Sum vs SumCopy: structural_hash should match (only the name differs)")
	}
	if sumC == copyC {
		t.Errorf("Sum vs SumCopy: content_hash should DIFFER (the name is in the text)")
	}
	if sumS != addS {
		t.Errorf("Sum vs Add: structural_hash should match (consistent rename), got %s vs %s", sumS, addS)
	}
	if sumC == addC {
		t.Errorf("Sum vs Add: content_hash should differ")
	}
	// A genuine logic change must result in a different structural hash.
	if sumS == mulS {
		t.Errorf("Sum vs Mul: structural_hash should differ (different shape)")
	}
}
