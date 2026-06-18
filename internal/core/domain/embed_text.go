// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package domain

import "strings"

// EmbedTextInput carries the node fields used for building text embeddings.
// Kind and SymbolPath are always populated; FilePath and Language are optional.
type EmbedTextInput struct {
	Kind       string
	SymbolPath string
	FilePath   string
	Language   string
	Signature  string
	Snippet    string
}

// EmbedTextVariant defines which fields are folded into the text embedding projection.
// EmbedVariantBaseline is used in production, while other variants are eval-only
// candidates. Keeping all variants in the domain package keeps the projection
// logic and label mapping cohesive.
type EmbedTextVariant int

const (
	// EmbedVariantBaseline represents the production projection format:
	// "<kind> <symbol_path> <file_path> <language>".
	EmbedVariantBaseline EmbedTextVariant = iota
	// EmbedVariantSignature appends the symbol signature to the baseline fields.
	EmbedVariantSignature
	// EmbedVariantSnippet appends a code snippet to the baseline fields.
	EmbedVariantSnippet
	// EmbedVariantBoth appends both the signature and code snippet.
	EmbedVariantBoth
)

// String returns the lowercase string representation of the variant, suitable for
// environment variables and reports.
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

// EmbedText builds the deterministic text projection for a node. The baseline
// variant joins non-empty fields with a single space. Non-baseline variants
// append signature and/or snippet fields, omitting any empty values to avoid
// introducing extra spacing. This function is shared between the production
// SQLite adapter and the evaluation harness to guarantee projection consistency.
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
