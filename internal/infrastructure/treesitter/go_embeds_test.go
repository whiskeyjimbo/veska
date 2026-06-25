// SPDX-License-Identifier: AGPL-3.0-only

package treesitter_test

import (
	"context"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/treesitter"
)

// relFor returns the embed rel whose target base name matches, or nil.
func relFor(rels []domain.UnresolvedTypeRel, target string) *domain.UnresolvedTypeRel {
	for i := range rels {
		if rels[i].TargetName == target {
			return &rels[i]
		}
	}
	return nil
}

func TestExtractEmbeds_StructPointerAndValue(t *testing.T) {
	src := []byte(`package foo

type Base struct{}
type Helper struct{}

type Server struct {
	*Base
	Helper
	name string
}
`)
	p := treesitter.NewGoParser()
	res, err := p.ParseFile(context.Background(), repoID, filePath, src)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	base := relFor(res.TypeRels, "Base")
	if base == nil {
		t.Fatal("expected embed of Base")
	}
	if !base.Pointer {
		t.Error("Base embed should be marked Pointer (*Base)")
	}
	if base.Kind != domain.EdgeEmbeds {
		t.Errorf("kind: got %q want EMBEDS", base.Kind)
	}
	val := relFor(res.TypeRels, "Helper")
	if val == nil {
		t.Fatal("expected embed of Helper")
	}
	if val.Pointer {
		t.Error("Helper embed should be value, not Pointer")
	}
	// The named field `name string` must NOT be an embed.
	if relFor(res.TypeRels, "string") != nil {
		t.Error("named field 'name string' must not be treated as an embed")
	}
}

func TestExtractEmbeds_SelectorQualified(t *testing.T) {
	src := []byte(`package foo

import "log"

type Server struct {
	*log.Logger
}
`)
	p := treesitter.NewGoParser()
	res, err := p.ParseFile(context.Background(), repoID, filePath, src)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	rel := relFor(res.TypeRels, "Logger")
	if rel == nil {
		t.Fatal("expected embed of Logger")
	}
	if rel.PkgQualifier != "log" {
		t.Errorf("pkg qualifier: got %q want log", rel.PkgQualifier)
	}
	if !rel.Pointer {
		t.Error("*log.Logger embed should be Pointer")
	}
}

func TestExtractEmbeds_InterfaceEmbedsInterface(t *testing.T) {
	src := []byte(`package foo

import "io"

type Reader interface {
	Read(p []byte) (int, error)
}

type ReadWriter interface {
	Reader
	io.Writer
}
`)
	p := treesitter.NewGoParser()
	res, err := p.ParseFile(context.Background(), repoID, filePath, src)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if relFor(res.TypeRels, "Reader") == nil {
		t.Error("expected ReadWriter to embed Reader")
	}
	w := relFor(res.TypeRels, "Writer")
	if w == nil {
		t.Fatal("expected ReadWriter to embed io.Writer")
	}
	if w.PkgQualifier != "io" {
		t.Errorf("Writer pkg qualifier: got %q want io", w.PkgQualifier)
	}
	// Interface method nodes must now carry a signature for IMPLEMENTS matching.
	read := findNodeByName(res.Nodes, "Reader.Read")
	if read == nil {
		t.Fatal("expected interface method node Reader.Read")
	}
	if read.Signature == nil || *read.Signature == "" {
		t.Error("interface method node should carry a signature")
	}
}
