// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

package tokenefficiency

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/pkoukk/tiktoken-go"
)

// EncodingName is the tiktoken encoding used for every token count. We
// pin to cl100k_base - GPT-4 / GPT-3.5-turbo / claude-flavoured estimate
// because it is the published default and the comparable baseline
// semble uses for its public figure. The exact encoding doesn't bias the
// ratio (veska and grep are tokenised by the same function); the choice
// matters only for cross-tool comparisons.
const EncodingName = "cl100k_base"

var (
	encoderOnce sync.Once
	encoderErr  error
	encoder     *tiktoken.Tiktoken
)

// CountTokens returns the cl100k_base token count for s. The encoder is
// cached after first use so repeated calls in a hot loop don't re-init.
func CountTokens(s string) (int, error) {
	encoderOnce.Do(func() {
		encoder, encoderErr = tiktoken.GetEncoding(EncodingName)
	})
	if encoderErr != nil {
		return 0, fmt.Errorf("tokenefficiency: encoder: %w", encoderErr)
	}
	if s == "" {
		return 0, nil
	}
	return len(encoder.Encode(s, nil, nil)), nil
}

// QueryFixture is one row in the benchmark input: a query, the text
// veska's search ought to retrieve (the "right answer" - i.e. the
// snippet content for the truth nodes), the set of ground-truth node
// ids, and the simulated filesystem grep would read against.
// FilesByPath holds every file in the corpus by absolute (or fake)
// path. NodeToFile maps node_id → file path so the stop-when-covered
// baseline can detect when a read has covered a truth target. Both
// maps are shared across the whole query set so callers populate them
// once when constructing the corpus.
type QueryFixture struct {
	Query    string
	Truth    map[string]struct{}
	GrepHits []string // file paths the simulator returned for this query
}

// SearchResult is the K-shaped output expected from veska. Each
// element carries the NodeID the engine returned plus the snippet bytes
// that would land in the agent's context. Only Snippet and NodeID are
// used by the harness - passing the full search.Result keeps callers
// straightforward.
type SearchResult struct {
	NodeID  string
	Snippet string
}

// BaselineMode names which grep+read assumption a token total came from.
type BaselineMode string

const (
	// BaselineStopWhenCovered: agent reads grep matches in rank order
	// and stops the moment one of the read files contains a ground-truth
	// target. This is the LOWER BOUND on grep+read tokens (and so the
	// LOWER BOUND on veska's savings ratio).
	BaselineStopWhenCovered BaselineMode = "stop_when_covered"
	// BaselineReadAllMatches: agent reads every file grep returned. This
	// is the UPPER BOUND on grep+read tokens (and the UPPER BOUND on
	// veska's savings).
	BaselineReadAllMatches BaselineMode = "read_all_matches"
)

// PerQuery captures one query's full token-efficiency picture: veska's
// recall + token cost paired with both bracketed baselines. Negative
// "savings" values mean grep+read used FEWER tokens for that query - a
// healthy bracket for queries where the grep keyword path happens to
// be cheap.
type PerQuery struct {
	Query            string
	Recall           float64
	VeskaTokens      int
	BaselineLoTokens int
	BaselineHiTokens int
	SavingsLoVsGrep  float64 // 1 - veska/stop_when_covered
	SavingsHiVsGrep  float64 // 1 - veska/read_all_matches
	BaselineLoRecall float64 // recall the lower-bound grep run achieved
	BaselineHiRecall float64 // typically 1.0 unless grep returned no files
}

// Result is the JSON envelope written to disk and the structure the
// human-readable summary line is derived from. All averages are
// arithmetic means over the per-query rows.
type Result struct {
	Queries             int        `json:"queries"`
	K                   int        `json:"k"`
	Tokenizer           string     `json:"tokenizer"`
	MeanRecall          float64    `json:"mean_recall"`
	MeanVeskaTokens     float64    `json:"mean_veska_tokens"`
	MeanGrepLoTokens    float64    `json:"mean_grep_lo_tokens"`
	MeanGrepHiTokens    float64    `json:"mean_grep_hi_tokens"`
	MeanSavingsLoVsGrep float64    `json:"mean_savings_lo_vs_grep"`
	MeanSavingsHiVsGrep float64    `json:"mean_savings_hi_vs_grep"`
	MeanGrepLoRecall    float64    `json:"mean_grep_lo_recall"`
	PerQuery            []PerQuery `json:"per_query"`
	CorpusNote          string     `json:"corpus_note"`
	// Embedder identifies which provider produced the retrieval-side
	// vectors. Recall numbers are only comparable across runs with the
	// same embedder; the harness prefers model2vec when its assets are
	// present and falls back to the deterministic FakeEmbedder
	// otherwise (the latter is plumbing-only, not a realistic recall
	// signal - published recall on the semantic corpus requires the
	// real model).
	Embedder  string    `json:"embedder"`
	Timestamp time.Time `json:"timestamp"`

	// Absolute denomination of the savings figure. The ratio fields
	// above are the right metric for comparing harness runs; these
	// translate the same data into something concrete a reader can
	// quote in a doc or budget against.
	//   TokensSavedPerQuery = mean(grep_mid - veska) per query
	//   TokensSavedOverConversation = TokensSavedPerQuery * ConversationQueries
	//   USDSavedOverConversation = TokensSavedOverConversation * USDPerMToken / 1e6
	TokensSavedPerQuery         float64 `json:"tokens_saved_per_query"`
	ConversationQueries         int     `json:"conversation_queries"`
	TokensSavedOverConversation float64 `json:"tokens_saved_over_conversation"`
	USDPerMToken                float64 `json:"usd_per_mtoken"`
	USDPriceLabel               string  `json:"usd_price_label"`
	USDSavedOverConversation    float64 `json:"usd_saved_over_conversation"`
}

// SummaryLine renders Result into the expected one-liner output.
// Phrasing follows the standard stated shape so documentation can quote
// it verbatim.
func (r Result) SummaryLine() string {
	return fmt.Sprintf(
		"Veska found the right code ~%.0f%% of the time, using about %.0f%% as many tokens as grep+read would have (range: %.0f–%.0f%% fewer, depending on how aggressively the agent reads grep matches; measured on %d queries).",
		r.MeanRecall*100,
		r.meanVeskaPctOfGrepMidpoint(),
		r.MeanSavingsLoVsGrep*100,
		r.MeanSavingsHiVsGrep*100,
		r.Queries,
	)
}

// TokensLine concretises the savings ratio. Per-query absolute, the
// per-conversation extrapolation (default 50 searches per chat), and a
// dollar estimate at a configurable rate. Pricing drifts month to
// month - the label is printed alongside so a stale number is loud.
func (r Result) TokensLine() string {
	return fmt.Sprintf(
		"~ savings: %s tokens/query · %s tokens over %d searches · $%.4f at $%g/Mtok (%s).",
		formatThousands(int(r.TokensSavedPerQuery)),
		formatThousands(int(r.TokensSavedOverConversation)),
		r.ConversationQueries,
		r.USDSavedOverConversation,
		r.USDPerMToken,
		r.USDPriceLabel,
	)
}

// FillAbsoluteSavings populates the concrete-tokens / per-conversation
// / dollar fields of Result from its already-aggregated ratio fields.
// conversationQueries is the assumed conversation length (defaults to 50;
// callers can override). usdPerMToken is the model's
// input-token rate ($ per million tokens); label names the rate for
// the printed line.
func (r *Result) FillAbsoluteSavings(conversationQueries int, usdPerMToken float64, label string) {
	mid := (r.MeanGrepLoTokens + r.MeanGrepHiTokens) / 2
	perQuery := mid - r.MeanVeskaTokens
	if perQuery < 0 {
		perQuery = 0
	}
	r.TokensSavedPerQuery = perQuery
	r.ConversationQueries = conversationQueries
	r.TokensSavedOverConversation = perQuery * float64(conversationQueries)
	r.USDPerMToken = usdPerMToken
	r.USDPriceLabel = label
	r.USDSavedOverConversation = r.TokensSavedOverConversation * usdPerMToken / 1_000_000
}

// formatThousands renders n with comma separators ("12,345"). Avoids a
// dependency on golang.org/x/text/message for a one-shot need.
func formatThousands(n int) string {
	if n < 0 {
		return "-" + formatThousands(-n)
	}
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}
	first := len(s) % 3
	if first == 0 {
		first = 3
	}
	var out strings.Builder
	out.WriteString(s[:first])
	for i := first; i < len(s); i += 3 {
		out.WriteString("," + s[i:i+3])
	}
	return out.String()
}

func (r Result) meanVeskaPctOfGrepMidpoint() float64 {
	mid := (r.MeanGrepLoTokens + r.MeanGrepHiTokens) / 2
	if mid <= 0 {
		return 0
	}
	return 100 * r.MeanVeskaTokens / mid
}

// SimulateGrepFilesWithMatches returns the sorted list of file paths
// whose content contains query as a case-sensitive substring. Mirrors
// `rg --files-with-matches '<query>'` for the simulated corpus where
// `query` is a multi-phrase blob (each phrase is treated as a
// substring; a file matches if ANY phrase appears). Splitting on
// `. ` keeps the per-phrase semantics aligned with how the semantic
// corpus joins phrases.
func SimulateGrepFilesWithMatches(query string, filesByPath map[string]string) []string {
	phrases := splitQueryPhrases(query)
	if len(phrases) == 0 {
		return nil
	}
	var out []string
	for path, body := range filesByPath {
		for _, p := range phrases {
			if strings.Contains(body, p) {
				out = append(out, path)
				break
			}
		}
	}
	sort.Strings(out)
	return out
}

func splitQueryPhrases(q string) []string {
	parts := strings.Split(q, ". ")
	out := parts[:0]
	for _, p := range parts {
		p = strings.TrimSpace(strings.TrimSuffix(p, "."))
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// TokensFor returns the token count of every file at the supplied
// paths, keyed by path. Skips paths missing from filesByPath.
func TokensFor(paths []string, filesByPath map[string]string) (map[string]int, error) {
	out := make(map[string]int, len(paths))
	for _, p := range paths {
		body, ok := filesByPath[p]
		if !ok {
			continue
		}
		n, err := CountTokens(body)
		if err != nil {
			return nil, err
		}
		out[p] = n
	}
	return out, nil
}

// BaselineGrep runs both modes against grepHits and returns the
// (loTokens, hiTokens, loRecall, hiRecall) tuple a per-query row needs.
// fileNodeIDs maps file_path → node_ids defined in that file so the
// stop-when-covered mode can detect a truth match.
func BaselineGrep(
	grepHits []string,
	fileTokens map[string]int,
	fileNodeIDs map[string][]string,
	truth map[string]struct{},
) (loTokens, hiTokens int, loRecall, hiRecall float64) {
	for _, p := range grepHits {
		hiTokens += fileTokens[p]
	}
	hiRecall = recallFromFiles(grepHits, fileNodeIDs, truth)

	// Stop-when-covered: walk grepHits in their (deterministic, sorted)
	// order, accumulating tokens, stop as soon as a read file contains
	// any truth node. Recall is binary (1 if any was covered, else 0)
	// since the agent stops after the first hit.
	for _, p := range grepHits {
		loTokens += fileTokens[p]
		for _, nid := range fileNodeIDs[p] {
			if _, ok := truth[nid]; ok {
				loRecall = 1
				return
			}
		}
	}
	return
}

func recallFromFiles(paths []string, fileNodeIDs map[string][]string, truth map[string]struct{}) float64 {
	if len(truth) == 0 {
		return 0
	}
	matched := 0
	for _, p := range paths {
		for _, nid := range fileNodeIDs[p] {
			if _, ok := truth[nid]; ok {
				matched++
			}
		}
	}
	// Cap at 1.0 - a single grep hit can cover many truth nodes.
	r := float64(matched) / float64(len(truth))
	if r > 1 {
		r = 1
	}
	return r
}

// VeskaTokens returns the token count of every snippet in results
// concatenated. Empty snippets contribute zero tokens.
func VeskaTokens(results []SearchResult) (int, error) {
	if len(results) == 0 {
		return 0, nil
	}
	var b strings.Builder
	for _, r := range results {
		b.WriteString(r.Snippet)
		b.WriteByte('\n')
	}
	return CountTokens(b.String())
}

// RecallAtK is the standard recall@K used by the recall harness, hoisted
// here to keep the package self-contained (the recall package isn't
// build-tag-compatible with our eval test). Matches recall.RecallAtK
// semantics: denominator is min(k, len(truth)) so a query with truth
// larger than k still tops out at 1.0.
func RecallAtK(hits []string, truth map[string]struct{}, k int) float64 {
	if k <= 0 || len(truth) == 0 || len(hits) == 0 {
		return 0
	}
	upper := min(len(hits), k)
	matched := 0
	for i := range upper {
		if _, ok := truth[hits[i]]; ok {
			matched++
		}
	}
	denom := min(len(truth), k)
	return float64(matched) / float64(denom)
}

// Mean returns the arithmetic mean of xs, or 0 for an empty input.
func Mean(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	var s float64
	for _, x := range xs {
		s += x
	}
	return s / float64(len(xs))
}

// MeanInt is Mean for int slices, returning the float average so a
// caller can hand it straight to the Result fields.
func MeanInt(xs []int) float64 {
	if len(xs) == 0 {
		return 0
	}
	var s int
	for _, x := range xs {
		s += x
	}
	return float64(s) / float64(len(xs))
}

// SavingsRatio returns 1 - veska/baseline. baseline <= 0 yields 0 so
// queries where grep didn't return anything don't make savings look
// artificially infinite.
func SavingsRatio(veskaTokens, baselineTokens int) float64 {
	if baselineTokens <= 0 {
		return 0
	}
	return 1 - float64(veskaTokens)/float64(baselineTokens)
}
