// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package model2vec

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// synthTokenizerJSON builds a minimal tokenizer.json fixture mimicking
// the BertNormalizer + BertPreTokenizer + WordPiece pipeline Model2Vec
// inherits from BAAI/bge-* base tokenizers. The vocab is tiny but
// captures the exercise of every algorithmic branch:
//
//	[UNK] [CLS] [SEP] [PAD]
//	parse config parser ##er ##ing func return
//
// Token IDs are assigned by vocab order - same convention HF uses.
func synthTokenizerJSON(t *testing.T) []byte {
	t.Helper()
	vocab := map[string]int{
		"[UNK]":  0,
		"[CLS]":  1,
		"[SEP]":  2,
		"[PAD]":  3,
		"parse":  4,
		"config": 5,
		"parser": 6,
		"##er":   7,
		"##ing":  8,
		"func":   9,
		"return": 10,
	}
	spec := map[string]any{
		"normalizer": map[string]any{
			"type":                 "BertNormalizer",
			"lowercase":            true,
			"strip_accents":        true,
			"handle_chinese_chars": true,
			"clean_text":           true,
		},
		"pre_tokenizer": map[string]any{
			"type": "BertPreTokenizer",
		},
		"model": map[string]any{
			"type":                      "WordPiece",
			"unk_token":                 "[UNK]",
			"continuing_subword_prefix": "##",
			"max_input_chars_per_word":  100,
			"vocab":                     vocab,
		},
	}
	b, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func TestNewTokenizer_AcceptsBertWordPiecePipeline(t *testing.T) {
	tk, err := newTokenizer(synthTokenizerJSON(t))
	if err != nil {
		t.Fatalf("newTokenizer: %v", err)
	}
	if tk == nil {
		t.Fatal("tokenizer is nil")
	}
	if tk.unkID() != 0 {
		t.Errorf("unk id: got %d, want 0", tk.unkID())
	}
}

// TestEncode_KnownVocabulary covers the happy path: tokens that are
// in the vocab encode to their IDs in input order.
func TestEncode_KnownVocabulary(t *testing.T) {
	tk, _ := newTokenizer(synthTokenizerJSON(t))
	got := tk.encode("parse config")
	want := []int{4, 5}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// TestEncode_WordPieceContinuation: "configing" decomposes as
// config + ##ing - both are in the vocab and the character split
// is exact (con-fig-i-n-g). The continuing-subword prefix is what
// lets WordPiece handle morphology.
func TestEncode_WordPieceContinuation(t *testing.T) {
	tk, _ := newTokenizer(synthTokenizerJSON(t))
	got := tk.encode("configing")
	want := []int{5, 8} // config + ##ing
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// TestEncode_UnknownTokenFallsBackToUNK: a word that has NO greedy
// WordPiece decomposition (no prefix in vocab) collapses to [UNK].
// Without this the encoder would return an empty list or panic on
// out-of-vocab input - every real-world tokeniser must handle OOV.
func TestEncode_UnknownTokenFallsBackToUNK(t *testing.T) {
	tk, _ := newTokenizer(synthTokenizerJSON(t))
	got := tk.encode("xyzqwerty")
	want := []int{0}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// TestEncode_Lowercasing: BertNormalizer.lowercase=true means the
// encoder must lowercase before vocab lookup. "Parse" should hit the
// "parse" vocab entry, not [UNK].
func TestEncode_Lowercasing(t *testing.T) {
	tk, _ := newTokenizer(synthTokenizerJSON(t))
	got := tk.encode("Parse")
	want := []int{4}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// TestEncode_PunctuationIsItsOwnToken: BertPreTokenizer splits on
// punctuation. Each punctuation char is then a separate word for the
// WordPiece pass - unknown to our synthetic vocab → [UNK].
func TestEncode_PunctuationIsItsOwnToken(t *testing.T) {
	tk, _ := newTokenizer(synthTokenizerJSON(t))
	got := tk.encode("parse, config")
	// parse, config → [4, [UNK], 5]
	want := []int{4, 0, 5}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// TestEncode_EmptyStringYieldsNoTokens: callers (Embed) rely on this
// to detect "empty input" via an empty token-id slice and return the
// canonical empty-vector instead of crashing the lookup.
func TestEncode_EmptyStringYieldsNoTokens(t *testing.T) {
	tk, _ := newTokenizer(synthTokenizerJSON(t))
	if got := tk.encode(""); len(got) != 0 {
		t.Errorf("empty input should yield no tokens, got %v", got)
	}
	if got := tk.encode("   \t  "); len(got) != 0 {
		t.Errorf("whitespace-only input should yield no tokens, got %v", got)
	}
}

// TestNewTokenizer_RejectsUnsupportedModel: BPE / Unigram aren't
// supported by this MVP - surface a clear error rather than fall back
// to a partial WordPiece path that produces garbage IDs.
func TestNewTokenizer_RejectsUnsupportedModel(t *testing.T) {
	spec := `{"model":{"type":"BPE","vocab":{},"merges":[]}}`
	_, err := newTokenizer([]byte(spec))
	if err == nil {
		t.Fatal("expected error for BPE model, got nil")
	}
	if !strings.Contains(err.Error(), "WordPiece") {
		t.Errorf("error should mention required model type: %v", err)
	}
}
