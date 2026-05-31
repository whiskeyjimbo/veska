package domain

import "testing"

func TestEmbedTextBaseline(t *testing.T) {
	in := EmbedTextInput{
		Kind:       "function",
		SymbolPath: "pkg.Thing",
		FilePath:   "pkg/thing.go",
		Language:   "go",
		Signature:  "func Thing() error",
		Snippet:    "return nil",
	}
	got := EmbedText(in, EmbedVariantBaseline)
	if want := "function pkg.Thing pkg/thing.go go"; got != want {
		t.Fatalf("baseline: got %q want %q", got, want)
	}
}

func TestEmbedTextSkipsEmptyTrailingFields(t *testing.T) {
	in := EmbedTextInput{Kind: "function", SymbolPath: "pkg.Thing"}
	got := EmbedText(in, EmbedVariantBaseline)
	if want := "function pkg.Thing"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestEmbedTextVariantsExtendBaseline(t *testing.T) {
	in := EmbedTextInput{
		Kind:       "function",
		SymbolPath: "pkg.Thing",
		FilePath:   "pkg/thing.go",
		Language:   "go",
		Signature:  "func Thing() error",
		Snippet:    "return nil",
	}
	base := EmbedText(in, EmbedVariantBaseline)

	sig := EmbedText(in, EmbedVariantSignature)
	if want := base + " func Thing() error"; sig != want {
		t.Fatalf("+signature: got %q want %q", sig, want)
	}
	snip := EmbedText(in, EmbedVariantSnippet)
	if want := base + " return nil"; snip != want {
		t.Fatalf("+snippet: got %q want %q", snip, want)
	}
	both := EmbedText(in, EmbedVariantBoth)
	if want := base + " func Thing() error return nil"; both != want {
		t.Fatalf("+both: got %q want %q", both, want)
	}
}

func TestEmbedTextEnrichmentEmptyFieldsCollapseToBaseline(t *testing.T) {
	in := EmbedTextInput{Kind: "function", SymbolPath: "pkg.Thing"}
	base := EmbedText(in, EmbedVariantBaseline)
	for _, v := range []EmbedTextVariant{EmbedVariantSignature, EmbedVariantSnippet, EmbedVariantBoth} {
		if got := EmbedText(in, v); got != base {
			t.Fatalf("variant %v with empty enrichment: got %q want baseline %q", v, got, base)
		}
	}
}

func TestEmbedTextVariantString(t *testing.T) {
	cases := map[EmbedTextVariant]string{
		EmbedVariantBaseline:  "baseline",
		EmbedVariantSignature: "+signature",
		EmbedVariantSnippet:   "+snippet",
		EmbedVariantBoth:      "+both",
	}
	for v, want := range cases {
		if got := v.String(); got != want {
			t.Fatalf("String(%d): got %q want %q", v, got, want)
		}
	}
}
