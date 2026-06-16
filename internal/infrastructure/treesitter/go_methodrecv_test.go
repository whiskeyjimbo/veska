package treesitter_test

import (
	"context"
	"testing"

	"github.com/whiskeyjimbo/veska/internal/core/domain"
	"github.com/whiskeyjimbo/veska/internal/infrastructure/treesitter"
)

// hasCallEdge reports whether result holds a CALLS edge from the node named
// srcName to the node named dstName.
func hasCallEdge(nodes []*domain.Node, edges []*domain.Edge, srcName, dstName string) bool {
	src := findNodeByName(nodes, srcName)
	dst := findNodeByName(nodes, dstName)
	if src == nil || dst == nil {
		return false
	}
	for _, e := range edges {
		if e.Kind == domain.EdgeCalls && e.Src == src.ID && e.Tgt == dst.ID {
			return true
		}
	}
	return false
}

// TestParseFile_MethodCall_ValueReceiverIdioms guards: a value /
// pointer-receiver method call `recv.Method` must resolve to a CALLS edge when
// the receiver is a plain typed PARAMETER or a COMPOSITE-LITERAL local — the two
// idioms that previously fell to the default (package-qualifier) branch and
// produced an unbindable UnresolvedCall, so any method tested via `recv.M`
// looked uncalled. All callers/callees are in one file so the same-package call
// rewrites to "T.Method" and resolves intra-file to a real edge.
func TestParseFile_MethodCall_ValueReceiverIdioms(t *testing.T) {
	src := []byte(`package shop

type Order struct{ Qty int }

func (o Order) Total() int { return o.Qty }

type Server struct{ Name string }

func (s *Server) Handle() string { return s.Name }

// typed value-parameter receiver: o.Total()
func Place(o Order) int { return o.Total() }

// composite-literal local receiver: x.Total()
func compute() int {
	x := Order{Qty: 2}
	return x.Total()
}

// pointer typed-parameter receiver: s.Handle()
func use(s *Server) string { return s.Handle() }

// address-of composite-literal local receiver: srv.Handle()
func build() string {
	srv := &Server{Name: "n"}
	return srv.Handle()
}
`)
	p := treesitter.NewGoParser()
	result, err := p.ParseFile(context.Background(), repoID, filePath, src)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	cases := []struct{ src, dst string }{
		{"Place", "Order.Total"},   // typed value param
		{"compute", "Order.Total"}, // composite-literal local
		{"use", "Server.Handle"},   // pointer typed param
		{"build", "Server.Handle"}, // &composite-literal local
	}
	for _, c := range cases {
		if !hasCallEdge(result.Nodes, result.Edges, c.src, c.dst) {
			t.Errorf("expected CALLS edge %s -> %s (solov2-d521); got unresolved=%+v", c.src, c.dst, result.UnresolvedCalls)
		}
	}
}
