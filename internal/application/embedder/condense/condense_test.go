package condense

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// fakeEmbedder maps each piece to a one-hot vector based on a leading
// tag of the form "[T]" / "[N]" / "[X]" / etc. Pieces sharing a tag get
// the same vector → cosine 1.0 with each other and 0.0 with anything
// else, which makes the centrality math deterministic and easy to
// reason about. An untagged piece maps to a zero vector (cosine 0 with
// everything, so it has the lowest possible centrality).
type fakeEmbedder struct {
	failOn string // if a piece contains this substring, Embed returns errFakeFailed
}

var errFakeFailed = errors.New("fake embed failure")

func (f *fakeEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	if f.failOn != "" && strings.Contains(text, f.failOn) {
		return nil, errFakeFailed
	}
	// Tag occupies a fixed prefix slot. Vectors are dim=26 so we can use
	// A-Z as tags without collision.
	vec := make([]float32, 26)
	if len(text) >= 3 && text[0] == '[' && text[2] == ']' {
		idx := int(text[1] - 'A')
		if idx >= 0 && idx < 26 {
			vec[idx] = 1
		}
	}
	return vec, nil
}

func (f *fakeEmbedder) ModelID() string { return "fake" }

// TestCondense_CodeBody_PicksTopicLines exercises case (a) from oo4q.1's
// acceptance criteria. A function body where five topic lines (two
// docstring lines, signature, body call, return) share a topic tag and
// three boilerplate err-returns share a noise tag. The one-hot
// embedder makes centrality purely a majority count, so T (5) wins
// over N (3); top-3 must be the three highest-centrality T lines.
// Order matters: top-3 by centrality and then by original index, so we
// expect the FIRST three T pieces (positions 0, 1, 2) - a tie among
// equal-centrality pieces breaks toward the earlier index, which keeps
// natural reading order (docstring before body).
func TestCondense_CodeBody_PicksTopicLines(t *testing.T) {
	pieces := []string{
		"[T] // Promote moves staged nodes into permanent storage.",
		"[T] // Returns ErrUnregisteredRepo if the repo isn't in the registry.",
		"[T] func (p *Promoter) Promote(ctx context.Context) error {",
		"[N] if err != nil { return err }",
		"[N] if err != nil { return err }",
		"[N] if err != nil { return err }",
		"[T] batch := p.buildBatch(ctx)",
		"[T] return p.store.Promote(ctx, batch)",
	}
	got, err := Condense(context.Background(), &fakeEmbedder{}, pieces, 3)
	if err != nil {
		t.Fatalf("Condense: %v", err)
	}
	want := []string{
		"[T] // Promote moves staged nodes into permanent storage.",
		"[T] // Returns ErrUnregisteredRepo if the repo isn't in the registry.",
		"[T] func (p *Promoter) Promote(ctx context.Context) error {",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("top-K mismatch\n got:  %q\n want: %q", got, want)
	}
}

// TestCondense_MajorityWinsCentrality: confirms the one-hot fake's
// centrality is purely majority-driven. With 3 N pieces vs 2 T pieces,
// top-1 must come from the N group regardless of where T appears.
// Among equal-centrality pieces, the earlier index wins (stable sort),
// so we expect the first N piece.
func TestCondense_MajorityWinsCentrality(t *testing.T) {
	pieces := []string{
		"[N] Many systems exist for different purposes.",
		"[T] Veska indexes a code graph and serves it over MCP.",
		"[T] Veska indexes code so agents can search it.",
		"[N] Some are old, some are new.",
		"[N] The world keeps turning regardless.",
	}
	got, err := Condense(context.Background(), &fakeEmbedder{}, pieces, 1)
	if err != nil {
		t.Fatalf("Condense: %v", err)
	}
	want := []string{"[N] Many systems exist for different purposes."}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("top-1 = %q, want %q", got, want)
	}
}

// TestCondense_TopicMajority: when the topic-tagged group has the
// majority, top-K should surface topic pieces. Confirms the centrality
// math reflects group dominance, which is the realistic case for a
// well-written paragraph where most sentences support one topic.
func TestCondense_TopicMajority(t *testing.T) {
	pieces := []string{
		"[N] Tangential aside.",
		"[T] Topic sentence.",
		"[T] Supporting detail one.",
		"[T] Supporting detail two.",
		"[T] Supporting detail three.",
	}
	got, err := Condense(context.Background(), &fakeEmbedder{}, pieces, 2)
	if err != nil {
		t.Fatalf("Condense: %v", err)
	}
	// Top-2 should both be T pieces (centrality 3/4 each vs 0 for N).
	// Order: original.
	want := []string{
		"[T] Topic sentence.",
		"[T] Supporting detail one.",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("top-2 mismatch\n got:  %q\n want: %q", got, want)
	}
}

// TestCondense_KGreaterThanN: case (c) - k > len(pieces) returns a
// copy of pieces unchanged (no condensation needed).
func TestCondense_KGreaterThanN(t *testing.T) {
	pieces := []string{"[T] one", "[T] two"}
	got, err := Condense(context.Background(), &fakeEmbedder{}, pieces, 10)
	if err != nil {
		t.Fatalf("Condense: %v", err)
	}
	if !reflect.DeepEqual(got, pieces) {
		t.Errorf("got %q, want %q", got, pieces)
	}
}

// TestCondense_KZero: case (d) - k <= 0 returns nil.
func TestCondense_KZero(t *testing.T) {
	for _, k := range []int{0, -1, -100} {
		got, err := Condense(context.Background(), &fakeEmbedder{}, []string{"[T] x"}, k)
		if err != nil {
			t.Fatalf("k=%d: %v", k, err)
		}
		if got != nil {
			t.Errorf("k=%d: got %q, want nil", k, got)
		}
	}
}

// TestCondense_SinglePiece: degenerate case - a one-piece input has no
// centrality to compute, so return it unchanged.
func TestCondense_SinglePiece(t *testing.T) {
	pieces := []string{"[T] solo"}
	got, err := Condense(context.Background(), &fakeEmbedder{}, pieces, 1)
	if err != nil {
		t.Fatalf("Condense: %v", err)
	}
	if !reflect.DeepEqual(got, pieces) {
		t.Errorf("got %q, want %q", got, pieces)
	}
}

// TestCondense_OrderPreserved: even when score-sorted ranking reverses
// the input order, the returned slice keeps original positions. This
// matters for downstream concatenation (signature before body reads
// better than body before signature).
func TestCondense_OrderPreserved(t *testing.T) {
	// [N] dominates centrally (3 N's vs 1 T), so top-3 = the three N
	// pieces, in original positions [0, 2, 4].
	pieces := []string{
		"[N] first",
		"[T] middle topic",
		"[N] second",
		"[T] another topic",
		"[N] third",
	}
	got, err := Condense(context.Background(), &fakeEmbedder{}, pieces, 3)
	if err != nil {
		t.Fatalf("Condense: %v", err)
	}
	want := []string{"[N] first", "[N] second", "[N] third"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("order not preserved\n got:  %q\n want: %q", got, want)
	}
}

// TestCondense_EmbedderError: an embedder failure aborts Condense and
// surfaces the error. Callers can fall back to the raw input.
func TestCondense_EmbedderError(t *testing.T) {
	pieces := []string{"[T] ok", "[T] BOOM", "[T] also ok"}
	_, err := Condense(context.Background(), &fakeEmbedder{failOn: "BOOM"}, pieces, 2)
	if !errors.Is(err, errFakeFailed) {
		t.Errorf("err = %v, want errFakeFailed", err)
	}
}
