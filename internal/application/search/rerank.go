// SPDX-License-Identifier: AGPL-3.0-only

package search

import (
	"path"
	"slices"
	"sort"
	"strings"
	"unicode"
)

// rerank applies post-fusion reranking signals to the
// hydrated candidate list before it is truncated to caller-k. Four
// signals contribute, all scaled by the candidate set's maxScore so
// they bite even on tight-clustered small-corpus distributions yet
// stay sub-noise on real corpora where raw vector scores already
// discriminate well:
//  1. definition boost - exact match on the trailing identifier of a
//     SymbolPath for a definitional Kind (function/method/type/.).
//     Lifts the chunk that DEFINES the queried symbol above chunks
//     that merely mention it.
//  2. identifier stems - split SymbolPath (and file basename) on
//     camelCase / snake_case / dotted boundaries; bonus per query
//     token that exactly equals a subword. "parse config" → matches
//     ParseConfig, configParser, parse_config_file.
//  3. file coherence - when multiple candidates share a file, each
//     candidate in that file gets a small bump (capped) so a tightly
//     clustered file outranks scattered isolated hits.
//  4. noise penalty - multiplicative dampener for *_test.go, legacy/,
//     examples/, vendor/, testdata/, *.d.ts paths.
//
// Stable-sorted by descending final score so equal-final-score input
// order (which is fused vector+lexical rank) is preserved.
func rerank(results []Result, query string) []Result {
	if len(results) == 0 {
		return results
	}
	tokens := tokenizeQuery(query)

	var maxScore float32
	for _, r := range results {
		if r.Score > maxScore {
			maxScore = r.Score
		}
	}

	fileCounts := make(map[string]int, len(results))
	for _, r := range results {
		if r.FilePath != "" {
			fileCounts[r.FilePath]++
		}
	}

	out := make([]Result, len(results))
	for i, r := range results {
		if maxScore > 0 && len(tokens) > 0 {
			bonus := definitionBonus(r, tokens, maxScore)
			bonus += identifierStemBonus(r, tokens, maxScore)
			bonus += verbSynonymBonus(r, tokens, maxScore)
			r.Score += bonus
		}
		if maxScore > 0 {
			r.Score += fileCoherenceBonus(r.FilePath, fileCounts, maxScore)
		}
		if isNoisePath(r.FilePath) {
			r.Score *= noiseMultiplier
		}
		if coarseKinds[r.Kind] {
			r.Score *= coarseKindMultiplier
		}
		out[i] = r
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Score > out[j].Score
	})
	return out
}

// definitionalKinds is the closed set of node kinds we treat as
// "definitions" for the definition-boost signal. Files, packages,
// modules, and fields are excluded - a file named save.go matching a
// query for "save" is a weak signal compared to a method literally
// named Save.
var definitionalKinds = map[string]bool{
	"function":  true,
	"method":    true,
	"type":      true,
	"struct":    true,
	"interface": true,
	"class":     true,
	"command":   true,
	"route":     true,
}

const definitionBonusFrac = 1.0

func definitionBonus(r Result, tokens []string, maxScore float32) float32 {
	if !definitionalKinds[r.Kind] {
		return 0
	}
	trail := strings.ToLower(trailingIdentifier(r.SymbolPath))
	if trail == "" {
		return 0
	}
	if slices.Contains(tokens, trail) {
		return definitionBonusFrac * maxScore
	}
	return 0
}

// trailingIdentifier returns the last dot-segment of a symbol path
// the identifier itself, stripped of receiver / namespace prefix.
func trailingIdentifier(symbolPath string) string {
	if i := strings.LastIndex(symbolPath, "."); i >= 0 {
		return symbolPath[i+1:]
	}
	return symbolPath
}

const identifierStemBonusFrac = 0.25

func identifierStemBonus(r Result, tokens []string, maxScore float32) float32 {
	matches := identifierStemMatches(tokens, r.SymbolPath, path.Base(r.FilePath))
	if matches == 0 {
		return 0
	}
	return identifierStemBonusFrac * maxScore * float32(matches)
}

// identifierStemMatches counts query tokens that exactly equal a
// lowercased subword of the symbol path or file basename (extension
// stripped). Exact subword equality, not prefix - "conf" does NOT
// match "config", because prefix matching turns common short tokens
// into wildcards that lift the entire candidate set uniformly.
func identifierStemMatches(tokens []string, symbolPath, fileBasename string) int {
	stems := splitIdentifier(symbolPath)
	if fileBasename != "" {
		if i := strings.LastIndex(fileBasename, "."); i > 0 {
			fileBasename = fileBasename[:i]
		}
		stems = append(stems, splitIdentifier(fileBasename)...)
	}
	if len(stems) == 0 {
		return 0
	}
	set := make(map[string]struct{}, len(stems))
	for _, s := range stems {
		set[s] = struct{}{}
	}
	n := 0
	for _, t := range tokens {
		if _, ok := set[t]; ok {
			n++
		}
	}
	return n
}

// splitIdentifier breaks an identifier into lowercased subwords on
// camelCase, PascalCase, snake_case, kebab-case, dotted, and slashed
// boundaries. Acronym runs split before the final capital before a
// lowercase: HTTPServer → [http, server], not [h,t,t,p,server] or
// [httpserver].
func splitIdentifier(s string) []string {
	if s == "" {
		return nil
	}
	runes := []rune(s)
	var parts []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			parts = append(parts, strings.ToLower(string(cur)))
			cur = cur[:0]
		}
	}
	for i, r := range runes {
		switch {
		case r == '_' || r == '.' || r == '/' || r == '-' || r == ' ':
			flush()
			continue
		case unicode.IsUpper(r):
			if i > 0 {
				prev := runes[i-1]
				if unicode.IsLower(prev) || unicode.IsDigit(prev) {
					flush()
				} else if unicode.IsUpper(prev) && i+1 < len(runes) && unicode.IsLower(runes[i+1]) {
					flush()
				}
			}
		}
		cur = append(cur, r)
	}
	flush()
	return parts
}

// verbSynonyms maps a query-side verb to API-side verbs callers commonly
// reach for the same action under. The lexical/embedding pipeline has
// no model of these clusters - a user types "register subcommand" but
// cobra exposes "AddCommand", so neither retriever surfaces the
// canonical answer well.
// Conservative on purpose: only well-known API-design clusters that
// appear repeatedly across Go libraries are listed. Adding a noun
// (e.g. "subcommand" → "command") would lift entire candidate sets
// uniformly and is left out.
var verbSynonyms = map[string][]string{
	// "register / install / attach X" → AddX
	"register": {"add", "install", "append"},
	"install":  {"add", "register"},
	"attach":   {"add", "append"},
	// "remove / delete / detach X" → RemoveX / DeleteX
	"remove": {"delete", "detach"},
	"delete": {"remove", "destroy"},
	"detach": {"remove", "detach"},
	// "retrieve / lookup X" → GetX / FetchX / FindX / ResolveX
	"lookup":   {"get", "find", "fetch", "resolve"},
	"retrieve": {"get", "fetch", "load", "read"},
	"fetch":    {"get", "retrieve", "load"},
	"find":     {"lookup", "resolve", "search"},
	// "build / create X" → NewX / MakeX / BuildX
	"create": {"new", "make", "build"},
	"make":   {"new", "create", "build"},
	"build":  {"new", "make", "create"},
}

// verbSynonymBonus boosts a result when its symbol's HEAD identifier
// (the leading subword - the "verb position" by Go API convention) is a
// known synonym of a query token. Gated on the head position so it
// doesn't false-fire on substring matches deep inside the identifier
// (e.g. "ItemsToAdd" is not registration).
// Bonus magnitude equals definitionBonusFrac × maxScore - same weight
// as an exact trailing-identifier match - so the canonical "AddCommand"
// for "register subcommand" can leapfrog larger sibling methods that
// gained a fusion edge from mentioning the noun in their body.
const verbSynonymBonusFrac = 1.0

func verbSynonymBonus(r Result, tokens []string, maxScore float32) float32 {
	if !definitionalKinds[r.Kind] {
		return 0
	}
	stems := splitIdentifier(r.SymbolPath)
	if len(stems) == 0 {
		return 0
	}
	// Head identifier is the FIRST subword of the trailing identifier
	// for "Command.AddCommand" splitIdentifier yields ["command","add",
	// "command"]; the trailing identifier "AddCommand" splits as
	// ["add","command"], so we take splitIdentifier of trailingIdentifier
	// to isolate the symbol's own verb position (skipping the receiver).
	trail := trailingIdentifier(r.SymbolPath)
	headStems := splitIdentifier(trail)
	if len(headStems) == 0 {
		return 0
	}
	head := headStems[0]
	for _, t := range tokens {
		syns, ok := verbSynonyms[t]
		if !ok {
			continue
		}
		if slices.Contains(syns, head) {
			return verbSynonymBonusFrac * maxScore
		}
	}
	return 0
}

const (
	fileCoherenceBonusFrac = 0.05
	fileCoherenceCapExtra  = 4
)

func fileCoherenceBonus(filePath string, fileCounts map[string]int, maxScore float32) float32 {
	if filePath == "" {
		return 0
	}
	n := fileCounts[filePath]
	if n <= 1 {
		return 0
	}
	extra := min(n-1, fileCoherenceCapExtra)
	return fileCoherenceBonusFrac * maxScore * float32(extra)
}

const noiseMultiplier = 0.6

// coarseKindMultiplier dampens coarse container kinds so a definition that
// implements the queried behavior outranks a container that merely matched on
// aggregate text (e.g. a package named "archive" winning an "extract from
// archive" query over the method that does it). Not a filter: the node keeps
// enough score to still surface for a genuine "where is package X" query.
const coarseKindMultiplier = 0.5

// coarseKinds are the file-spanning container kinds demoted by the signal
// above. Symbol-grain kinds (function/method/type/...) are never demoted.
var coarseKinds = map[string]bool{
	"package": true,
	"file":    true,
	"module":  true,
}

// noiseSuffixes / noiseSubstrings are the file-path patterns demoted
// by the noise-penalty signal. Conservative on purpose: a false noise
// classification permanently hides a relevant result, while missing
// noise just lets it tie with prod code.
var noiseSuffixes = []string{
	"_test.go",
	".d.ts",
}

var noiseSubstrings = []string{
	"/legacy/",
	"/examples/",
	"/vendor/",
	"/testdata/",
}

func isNoisePath(p string) bool {
	if p == "" {
		return false
	}
	lower := strings.ToLower(p)
	for _, suf := range noiseSuffixes {
		if strings.HasSuffix(lower, suf) {
			return true
		}
	}
	for _, sub := range noiseSubstrings {
		if strings.Contains(lower, sub) {
			return true
		}
	}
	return false
}
