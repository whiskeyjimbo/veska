// SPDX-FileCopyrightText: 2026 Jeff Rose
// SPDX-License-Identifier: AGPL-3.0-only

//go:build eval

// Package neardup holds the near-duplicate threshold-calibration harness
// The corpus is hand-curated real Go functions grouped into
// families, each with mechanically-edited near-duplicate variants, so the
// transformation (not synthetic prose) defines "clone". Three relationship
// tiers are derived from it:
//
//	neardup: an original and a mechanical edit of it (rename, comment,
//	  reformat, statement reorder) - semantically identical code.
//	related: two DIFFERENT real functions in the same family/domain
//	  auto-link's "merely related" band.
//	unrelated: two functions from different families.
//
// The harness embeds every text through a real provider, scores sampled
// pairs through the production memvec path (1/(1+L2^2)), and reports the
// per-tier score distributions. The threshold question is qualitative: do
// the neardup and related distributions separate, or overlap?
package neardup

// base is one source function plus its near-duplicate variants. original and
// every variant are byte-distinct (so content_hash differs - these are
// NEAR, not exact, clones) but semantically the same code.
type base struct {
	id       string
	family   string
	original string
	variants []string // mechanical edits of original: same logic, different text
}

// corpus is the curated set. Families: strings, mathx, slices. Each family
// has three distinct functions; each function has two near-dup variants.
var corpus = []base{
	// ── family: strings ──────────────────────────────────────────────────
	{
		id: "reverse", family: "strings",
		original: `func Reverse(s string) string {
	r := []rune(s)
	for i, j := 0, len(r)-1; i < j; i, j = i+1, j-1 {
		r[i], r[j] = r[j], r[i]
	}
	return string(r)
}`,
		variants: []string{
			// rename identifiers
			`func Reverse(in string) string {
	runes := []rune(in)
	for lo, hi := 0, len(runes)-1; lo < hi; lo, hi = lo+1, hi-1 {
		runes[lo], runes[hi] = runes[hi], runes[lo]
	}
	return string(runes)
}`,
			// add doc comment + reformat
			`// Reverse returns s with its runes in reverse order.
func Reverse(s string) string {
	r := []rune(s)
	for i, j := 0, len(r)-1; i < j; i, j = i+1, j-1 {
		r[i], r[j] = r[j], r[i]
	}
	return string(r)
}`,
		},
	},
	{
		id: "palindrome", family: "strings",
		original: `func IsPalindrome(s string) bool {
	r := []rune(s)
	for i, j := 0, len(r)-1; i < j; i, j = i+1, j-1 {
		if r[i] != r[j] {
			return false
		}
	}
	return true
}`,
		variants: []string{
			`func IsPalindrome(text string) bool {
	chars := []rune(text)
	for a, b := 0, len(chars)-1; a < b; a, b = a+1, b-1 {
		if chars[a] != chars[b] {
			return false
		}
	}
	return true
}`,
			`// IsPalindrome reports whether s reads the same forwards and backwards.
func IsPalindrome(s string) bool {
	r := []rune(s)

	for i, j := 0, len(r)-1; i < j; i, j = i+1, j-1 {
		if r[i] != r[j] {
			return false
		}
	}
	return true
}`,
		},
	},
	{
		id: "vowels", family: "strings",
		original: `func CountVowels(s string) int {
	n := 0
	for _, c := range s {
		switch c {
		case 'a', 'e', 'i', 'o', 'u':
			n++
		}
	}
	return n
}`,
		variants: []string{
			`func CountVowels(word string) int {
	count := 0
	for _, ch := range word {
		switch ch {
		case 'a', 'e', 'i', 'o', 'u':
			count++
		}
	}
	return count
}`,
			`// CountVowels returns the number of lowercase vowels in s.
func CountVowels(s string) int {
	n := 0
	for _, c := range s {
		switch c {
		case 'a', 'e', 'i', 'o', 'u':
			n++
		}
	}
	return n
}`,
		},
	},
	// ── family: mathx ────────────────────────────────────────────────────
	{
		id: "gcd", family: "mathx",
		original: `func GCD(a, b int) int {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}`,
		variants: []string{
			`func GCD(x, y int) int {
	for y != 0 {
		x, y = y, x%y
	}
	return x
}`,
			`// GCD returns the greatest common divisor of a and b.
func GCD(a, b int) int {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}`,
		},
	},
	{
		id: "factorial", family: "mathx",
		original: `func Factorial(n int) int {
	result := 1
	for i := 2; i <= n; i++ {
		result *= i
	}
	return result
}`,
		variants: []string{
			`func Factorial(num int) int {
	acc := 1
	for k := 2; k <= num; k++ {
		acc *= k
	}
	return acc
}`,
			`// Factorial returns n! for non-negative n.
func Factorial(n int) int {
	result := 1
	for i := 2; i <= n; i++ {
		result *= i
	}
	return result
}`,
		},
	},
	{
		id: "absint", family: "mathx",
		original: `func AbsInt(n int) int {
	if n < 0 {
		return -n
	}
	return n
}`,
		variants: []string{
			`func AbsInt(value int) int {
	if value < 0 {
		return -value
	}
	return value
}`,
			`// AbsInt returns the absolute value of n.
func AbsInt(n int) int {
	if n < 0 {
		return -n
	}
	return n
}`,
		},
	},
	// ── family: slices ───────────────────────────────────────────────────
	{
		id: "contains", family: "slices",
		original: `func Contains(xs []int, target int) bool {
	for _, x := range xs {
		if x == target {
			return true
		}
	}
	return false
}`,
		variants: []string{
			`func Contains(items []int, want int) bool {
	for _, it := range items {
		if it == want {
			return true
		}
	}
	return false
}`,
			`// Contains reports whether target appears in xs.
func Contains(xs []int, target int) bool {
	for _, x := range xs {
		if x == target {
			return true
		}
	}
	return false
}`,
		},
	},
	{
		id: "sum", family: "slices",
		original: `func SumInts(xs []int) int {
	total := 0
	for _, x := range xs {
		total += x
	}
	return total
}`,
		variants: []string{
			`func SumInts(nums []int) int {
	sum := 0
	for _, n := range nums {
		sum += n
	}
	return sum
}`,
			`// SumInts returns the sum of every element in xs.
func SumInts(xs []int) int {
	total := 0
	for _, x := range xs {
		total += x
	}
	return total
}`,
		},
	},
	{
		id: "max", family: "slices",
		original: `func MaxInt(xs []int) int {
	m := xs[0]
	for _, x := range xs[1:] {
		if x > m {
			m = x
		}
	}
	return m
}`,
		variants: []string{
			`func MaxInt(values []int) int {
	best := values[0]
	for _, v := range values[1:] {
		if v > best {
			best = v
		}
	}
	return best
}`,
			`// MaxInt returns the largest element of xs (xs must be non-empty).
func MaxInt(xs []int) int {
	m := xs[0]
	for _, x := range xs[1:] {
		if x > m {
			m = x
		}
	}
	return m
}`,
		},
	},
}

// labeledText is one corpus entry flattened: a unique id and its source text.
type labeledText struct {
	id     string
	family string
	baseID string
	text   string
}

// flatten expands the corpus into one labeledText per original and variant.
func flatten() []labeledText {
	var out []labeledText
	for _, b := range corpus {
		out = append(out, labeledText{id: b.id + "_orig", family: b.family, baseID: b.id, text: b.original})
		for i, v := range b.variants {
			out = append(out, labeledText{
				id:     b.id + "_var" + string(rune('1'+i)),
				family: b.family,
				baseID: b.id,
				text:   v,
			})
		}
	}
	return out
}

// tier classifies the relationship between two labeledTexts.
type tier int

const (
	tierNearDup tier = iota
	tierRelated
	tierUnrelated
)

func (t tier) String() string {
	switch t {
	case tierNearDup:
		return "neardup"
	case tierRelated:
		return "related"
	default:
		return "unrelated"
	}
}

// classify returns the tier of an (a, b) pair: same base = neardup, same
// family but different base = related, different family = unrelated.
func classify(a, b labeledText) tier {
	switch {
	case a.baseID == b.baseID:
		return tierNearDup
	case a.family == b.family:
		return tierRelated
	default:
		return tierUnrelated
	}
}
