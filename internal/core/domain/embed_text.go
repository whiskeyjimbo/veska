package domain

import "strings"

// EmbedTextInput carries the node fields that the embed-text projection
// can draw on. Kind and SymbolPath are always populated; FilePath and
// Language may be empty (the parser may leave language unset). Signature
// and Snippet are optional enrichment fields — empty unless a variant
// that consumes them is selected.
type EmbedTextInput struct {
	Kind       string
	SymbolPath string
	FilePath   string
	Language   string
	Signature  string
	Snippet    string
}

// EmbedTextVariant selects which fields the projection folds into the
// embed text. EmbedVariantBaseline is the ONLY value used in production
// (sqlite embedding_refs_repo); EmbedVariantSignature/Snippet/Both are
// eval-only candidates exercised solely by the recall sweep
// (tools/loadtest/recallprojection).
//
// The non-baseline variants deliberately stay in the domain rather than
// moving next to the eval harness: the variant enum, its String() labels,
// and the EmbedText projection switch are one cohesive contract, and the
// eval sweep's whole job is to compare candidate projections against the
// production baseline through that shared surface. Splitting them would
// fragment a single switch across two packages for no production benefit
// (decision recorded: solov2-xde2.15). Revisit only if eval ownership moves.
type EmbedTextVariant int

const (
	// EmbedVariantBaseline is the production projection:
	// "<kind> <symbol_path> <file_path> <language>".
	EmbedVariantBaseline EmbedTextVariant = iota
	// EmbedVariantSignature folds the symbol signature in after language.
	EmbedVariantSignature
	// EmbedVariantSnippet folds a code snippet in after language.
	EmbedVariantSnippet
	// EmbedVariantBoth folds both signature and snippet in.
	EmbedVariantBoth
)

// String returns the stable lowercase label for a variant, suitable for
// env knobs and report rows.
func (v EmbedTextVariant) String() string {
	switch v {
	case EmbedVariantSignature:
		return "+signature"
	case EmbedVariantSnippet:
		return "+snippet"
	case EmbedVariantBoth:
		return "+both"
	default:
		return "baseline"
	}
}

// EmbedText builds the deterministic Embed-input projection for a node.
//
// The baseline variant joins the non-empty parts of
// "<kind> <symbol_path> <file_path> <language>" with a single space —
// the exact projection the production FetchPending path produces.
// kind and symbolPath are always present; filePath and language may be
// empty. Enrichment variants append signature and/or snippet after the
// baseline fields, again skipping empty parts so a node missing the
// enrichment field projects identically to baseline.
//
// This function is the single shared definition consumed both by the
// production sqlite adapter and by the recall-projection eval harness so
// a projection change is measured against exactly what production emits.
func EmbedText(in EmbedTextInput, variant EmbedTextVariant) string {
	parts := make([]string, 0, 6)
	for _, p := range []string{in.Kind, in.SymbolPath, in.FilePath, in.Language} {
		if p != "" {
			parts = append(parts, p)
		}
	}
	if variant == EmbedVariantSignature || variant == EmbedVariantBoth {
		if in.Signature != "" {
			parts = append(parts, in.Signature)
		}
	}
	if variant == EmbedVariantSnippet || variant == EmbedVariantBoth {
		if in.Snippet != "" {
			parts = append(parts, in.Snippet)
		}
	}
	return strings.Join(parts, " ")
}
