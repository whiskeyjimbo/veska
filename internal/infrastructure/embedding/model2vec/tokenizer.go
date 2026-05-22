package model2vec

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode"
)

// tokenizer is a minimal HuggingFace tokenizer.json reader that
// supports the exact pipeline Model2Vec's distill targets ship with:
// BertNormalizer + BertPreTokenizer + WordPiece. That covers
// minishlab/potion-* (distilled from BAAI/bge-* base models).
// BPE and Unigram are explicitly rejected — surfacing the limit at
// load time beats producing garbage token IDs at query time.
type tokenizer struct {
	vocab map[string]int
	// continuingPrefix is the subword continuation marker, typically
	// "##". WordPiece prepends it to every non-initial subword during
	// greedy longest-match.
	continuingPrefix string
	unk              int
	maxCharsPerWord  int

	// normaliser flags from BertNormalizer config.
	lowercase    bool
	stripAccents bool
}

type tokenizerSpec struct {
	Normalizer   json.RawMessage `json:"normalizer"`
	PreTokenizer json.RawMessage `json:"pre_tokenizer"`
	Model        json.RawMessage `json:"model"`
}

type bertNormalizerCfg struct {
	Type         string `json:"type"`
	Lowercase    bool   `json:"lowercase"`
	StripAccents bool   `json:"strip_accents"`
}

type wordPieceModelCfg struct {
	Type                    string         `json:"type"`
	UnkToken                string         `json:"unk_token"`
	ContinuingSubwordPrefix string         `json:"continuing_subword_prefix"`
	MaxInputCharsPerWord    int            `json:"max_input_chars_per_word"`
	Vocab                   map[string]int `json:"vocab"`
}

// newTokenizer parses a tokenizer.json payload. The caller is
// responsible for fetching the bytes (embedded fixture or first-run
// download); this function is a pure decoder.
func newTokenizer(jsonBytes []byte) (*tokenizer, error) {
	var spec tokenizerSpec
	if err := json.Unmarshal(jsonBytes, &spec); err != nil {
		return nil, fmt.Errorf("tokenizer: parse json: %w", err)
	}

	var model wordPieceModelCfg
	if len(spec.Model) == 0 {
		return nil, fmt.Errorf("tokenizer: missing 'model' section")
	}
	if err := json.Unmarshal(spec.Model, &model); err != nil {
		return nil, fmt.Errorf("tokenizer: parse model: %w", err)
	}
	if model.Type != "WordPiece" {
		return nil, fmt.Errorf("tokenizer: only WordPiece supported (got %q)", model.Type)
	}
	if len(model.Vocab) == 0 {
		return nil, fmt.Errorf("tokenizer: vocab is empty")
	}
	if model.ContinuingSubwordPrefix == "" {
		model.ContinuingSubwordPrefix = "##"
	}
	if model.MaxInputCharsPerWord == 0 {
		model.MaxInputCharsPerWord = 100
	}
	unkID, ok := model.Vocab[model.UnkToken]
	if !ok {
		return nil, fmt.Errorf("tokenizer: unk_token %q not in vocab", model.UnkToken)
	}

	tk := &tokenizer{
		vocab:            model.Vocab,
		continuingPrefix: model.ContinuingSubwordPrefix,
		unk:              unkID,
		maxCharsPerWord:  model.MaxInputCharsPerWord,
	}

	// BertNormalizer config is optional — older tokenizer.json variants
	// omit the section entirely. Default to lowercase=true to match
	// the dominant bge-* base behaviour.
	if len(spec.Normalizer) > 0 {
		var n bertNormalizerCfg
		if err := json.Unmarshal(spec.Normalizer, &n); err != nil {
			return nil, fmt.Errorf("tokenizer: parse normalizer: %w", err)
		}
		tk.lowercase = n.Lowercase
		tk.stripAccents = n.StripAccents
	} else {
		tk.lowercase = true
	}
	return tk, nil
}

func (t *tokenizer) unkID() int { return t.unk }

// encode normalises text, splits via BertPreTokenizer rules (whitespace
// + punctuation), and emits WordPiece IDs.
func (t *tokenizer) encode(text string) []int {
	text = t.normalise(text)
	words := bertPreTokenize(text)
	if len(words) == 0 {
		return nil
	}
	var out []int
	for _, w := range words {
		if w == "" {
			continue
		}
		out = append(out, t.wordPiece(w)...)
	}
	return out
}

// normalise applies the BertNormalizer steps we care about: lowercase
// (cheap, language-agnostic) and accent-stripping (decompose to NFD
// and drop combining marks). clean_text + handle_chinese_chars are
// out of scope; they affect punctuation surface but not the IDs that
// land in the embedding lookup for typical English/code input.
func (t *tokenizer) normalise(s string) string {
	if t.lowercase {
		s = strings.ToLower(s)
	}
	if t.stripAccents {
		var b strings.Builder
		b.Grow(len(s))
		for _, r := range s {
			if unicode.Is(unicode.Mn, r) {
				continue
			}
			b.WriteRune(r)
		}
		s = b.String()
	}
	return s
}

// bertPreTokenize splits on whitespace AND on punctuation, with each
// punctuation char becoming its own token. Matches the BertPreTokenizer
// behaviour HuggingFace ships.
func bertPreTokenize(s string) []string {
	var out []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
		}
	}
	for _, r := range s {
		switch {
		case unicode.IsSpace(r):
			flush()
		case unicode.IsPunct(r) || unicode.IsSymbol(r):
			flush()
			out = append(out, string(r))
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return out
}

// wordPiece runs greedy longest-match decomposition over word against
// the vocab. On the FIRST sub-token, no continuing-subword prefix is
// applied; on every subsequent sub-token, the prefix is. When no
// prefix of the remaining word matches the vocab, the WHOLE word
// resolves to [UNK] — this matches HuggingFace's reference algorithm
// (it doesn't emit a partial split followed by [UNK]).
func (t *tokenizer) wordPiece(word string) []int {
	if len(word) > t.maxCharsPerWord {
		return []int{t.unk}
	}
	chars := []rune(word)
	var ids []int
	start := 0
	for start < len(chars) {
		end := len(chars)
		var matchID int
		matched := false
		for end > start {
			sub := string(chars[start:end])
			if start > 0 {
				sub = t.continuingPrefix + sub
			}
			if id, ok := t.vocab[sub]; ok {
				matchID = id
				matched = true
				break
			}
			end--
		}
		if !matched {
			return []int{t.unk}
		}
		ids = append(ids, matchID)
		start = end
	}
	if len(ids) == 0 {
		return []int{t.unk}
	}
	return ids
}
