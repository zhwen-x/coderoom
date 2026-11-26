// Package similarity finds submissions that look like copies of each other.
//
// The method is winnowing (Schleimer, Wilkerson and Aiken, 2003), the same
// idea behind MOSS. Source is reduced to a stream of normalised tokens, every
// run of k tokens is hashed, and a sample of those hashes is kept by taking
// the smallest in each sliding window. Two properties make that sample worth
// keeping instead of all of them: any shared passage longer than a known
// threshold is guaranteed to produce a shared fingerprint, and the sample is
// positional - inserting code at the top does not shift every later hash.
//
// Normalising the token stream is what makes the comparison about structure
// rather than spelling. Identifiers all become the same token, so renaming
// every variable changes nothing; numbers and string contents likewise, since
// a problem statement dictates most of those anyway. Keywords and operators
// are kept, because those are the shape of the program.
//
// What this cannot do is tell copying from convergence. Two correct answers to
// "read two integers and print their sum" are the same program, and no
// algorithm can say otherwise. That is why MinTokens exists, why the shared
// count is reported next to the ratio, and why the teacher-facing wording is
// "相似" rather than "抄袭".
package similarity

import (
	"hash/fnv"
	"sort"
	"strings"
)

// Tuning. These are the values the original paper's guarantee is stated with:
// any shared passage of at least K+W-1 tokens produces at least one shared
// fingerprint, and roughly 2/(W+1) of all hashes are kept.
const (
	// K is the token-run length that gets hashed. Small enough that a copied
	// loop body is caught, large enough that common idioms are not.
	K = 9
	// W is the winnowing window. With K=9 the guarantee covers any shared
	// passage of 16 tokens or more - a few lines of code.
	W = 8
	// MinTokens is the shortest submission worth comparing at all. Below this
	// every correct answer looks like every other one, because the problem
	// leaves no room to differ, and reporting that wastes a teacher's
	// attention on the one thing they cannot act on.
	MinTokens = 60
)

// Doc is one submission reduced to what the comparison needs.
type Doc struct {
	ID string
	// Tokens is the normalised token count, which is how the caller knows
	// whether a low score means "different" or "too short to tell".
	Tokens int
	// Fingerprints is sorted and de-duplicated, so comparison is a merge.
	Fingerprints []uint64
	// TooShort is set when the submission fell below MinTokens. Such a doc is
	// still returned, so the teacher can be told it was skipped rather than
	// left to assume it was clean.
	TooShort bool
}

// Match is the comparison of two documents.
type Match struct {
	A, B string `json:"-"`
	// Shared is the number of fingerprints both documents contain. A ratio on
	// its own hides the difference between "three fragments in common" and
	// "ninety", which is exactly the difference a teacher is judging.
	Shared int `json:"shared"`
	// Score is the share of the smaller document found in the larger, as a
	// percentage. Containment rather than Jaccard: a short answer pasted into
	// a longer one should score high, and Jaccard would dilute it away.
	Score int `json:"score"`
}

// Analyse fingerprints one submission. lang selects the comment and literal
// syntax; anything unrecognised is treated as C-like, which is the safe
// direction - at worst a few extra tokens survive.
func Analyse(id, lang, source string) Doc {
	tokens := Tokenize(lang, source)
	d := Doc{ID: id, Tokens: len(tokens)}
	if len(tokens) < MinTokens {
		d.TooShort = true
		return d
	}
	d.Fingerprints = winnow(hashGrams(tokens, K), W)
	return d
}

// Compare scores two fingerprinted documents.
func Compare(a, b Doc) Match {
	m := Match{A: a.ID, B: b.ID}
	if len(a.Fingerprints) == 0 || len(b.Fingerprints) == 0 {
		return m
	}
	m.Shared = sharedCount(a.Fingerprints, b.Fingerprints)
	smaller := len(a.Fingerprints)
	if len(b.Fingerprints) < smaller {
		smaller = len(b.Fingerprints)
	}
	m.Score = int(float64(m.Shared) / float64(smaller) * 100)
	if m.Score > 100 {
		m.Score = 100
	}
	return m
}

// sharedCount merges two sorted, de-duplicated slices.
func sharedCount(a, b []uint64) int {
	n, i, j := 0, 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] == b[j]:
			n++
			i++
			j++
		case a[i] < b[j]:
			i++
		default:
			j++
		}
	}
	return n
}

// hashGrams hashes every run of k consecutive tokens.
func hashGrams(tokens []string, k int) []uint64 {
	if len(tokens) < k {
		return nil
	}
	out := make([]uint64, 0, len(tokens)-k+1)
	for i := 0; i+k <= len(tokens); i++ {
		h := fnv.New64a()
		for j := i; j < i+k; j++ {
			h.Write([]byte(tokens[j]))
			h.Write([]byte{0}) // separator, so "ab"+"c" differs from "a"+"bc"
		}
		out = append(out, h.Sum64())
	}
	return out
}

// winnow keeps the smallest hash in each window of w.
//
// Ties resolve to the rightmost occurrence, and a hash already selected is not
// selected again for an overlapping window. Both details come from the paper
// and together are what make the sample positional: the same passage yields
// the same fingerprints wherever it appears in the file.
func winnow(hashes []uint64, w int) []uint64 {
	if len(hashes) == 0 {
		return nil
	}
	if w < 1 {
		w = 1
	}
	if len(hashes) < w {
		// Too short to winnow: keep the minimum so the document still has one
		// fingerprint rather than none.
		min := hashes[0]
		for _, h := range hashes[1:] {
			if h < min {
				min = h
			}
		}
		return []uint64{min}
	}

	var picked []uint64
	lastIdx := -1
	for start := 0; start+w <= len(hashes); start++ {
		minIdx := start
		for i := start + 1; i < start+w; i++ {
			// <= keeps the rightmost minimum, as the paper specifies.
			if hashes[i] <= hashes[minIdx] {
				minIdx = i
			}
		}
		if minIdx != lastIdx {
			picked = append(picked, hashes[minIdx])
			lastIdx = minIdx
		}
	}
	return dedupSorted(picked)
}

func dedupSorted(in []uint64) []uint64 {
	if len(in) == 0 {
		return nil
	}
	sort.Slice(in, func(i, j int) bool { return in[i] < in[j] })
	out := in[:1]
	for _, v := range in[1:] {
		if v != out[len(out)-1] {
			out = append(out, v)
		}
	}
	return out
}

// ----------------------------------------------------------------- tokenizer

// cKeywords and pyKeywords are kept verbatim in the token stream. Everything
// else that looks like a name collapses to one token, which is what makes
// renaming variables pointless.
var cKeywords = words(`
	alignas alignof and and_eq asm auto bitand bitor bool break case catch char
	class compl const consteval constexpr constinit const_cast continue co_await
	co_return co_yield decltype default delete do double dynamic_cast else enum
	explicit export extern false float for friend goto if inline int long mutable
	namespace new noexcept not not_eq nullptr operator or or_eq private protected
	public register reinterpret_cast requires return short signed sizeof static
	static_assert static_cast struct switch template this thread_local throw true
	try typedef typeid typename union unsigned using virtual void volatile wchar_t
	while xor xor_eq`)

var pyKeywords = words(`
	False None True and as assert async await break class continue def del elif
	else except finally for from global if import in is lambda nonlocal not or
	pass raise return try while with yield match case`)

func words(s string) map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(s) {
		m[w] = true
	}
	return m
}

// Tokenize reduces source to the normalised stream the fingerprints are built
// from. It never panics: students submit anything, including files that are
// not the language they claimed.
func Tokenize(lang, source string) []string {
	python := strings.HasPrefix(strings.ToLower(lang), "py")
	keywords := cKeywords
	if python {
		keywords = pyKeywords
	}

	src := []rune(source)
	out := make([]string, 0, len(src)/3)
	for i := 0; i < len(src); {
		c := src[i]

		switch {
		case c == ' ' || c == '\t' || c == '\r' || c == '\n' || c == '\f' || c == '\v':
			i++

		// Comments carry no program structure, and are the first thing someone
		// changes when passing code along.
		case python && c == '#':
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case !python && c == '/' && i+1 < len(src) && src[i+1] == '/':
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case !python && c == '/' && i+1 < len(src) && src[i+1] == '*':
			i += 2
			for i+1 < len(src) && !(src[i] == '*' && src[i+1] == '/') {
				i++
			}
			if i+1 < len(src) {
				i += 2
			} else {
				i = len(src)
			}

		// String and character literals become one token. Their contents are
		// usually dictated by the problem statement, so comparing them would
		// find agreement where there was never a choice.
		case c == '"' || c == '\'':
			i = skipString(src, i)
			out = append(out, "STR")

		case isDigit(c):
			for i < len(src) && (isDigit(src[i]) || isIdentRune(src[i]) || src[i] == '.') {
				i++
			}
			out = append(out, "NUM")

		case isIdentStart(c):
			start := i
			for i < len(src) && isIdentRune(src[i]) {
				i++
			}
			word := string(src[start:i])
			if keywords[word] {
				out = append(out, word)
			} else {
				out = append(out, "ID")
			}

		default:
			// Multi-character operators stay together: "<<" is one idea, not
			// two, and splitting them would let reformatting change the stream.
			if op := matchOperator(src, i); op != "" {
				out = append(out, op)
				i += len([]rune(op))
			} else {
				out = append(out, string(c))
				i++
			}
		}
	}
	return out
}

var operators = []string{
	"<<=", ">>=", "...", "->*", "<=>",
	"==", "!=", "<=", ">=", "&&", "||", "++", "--", "+=", "-=", "*=", "/=",
	"%=", "&=", "|=", "^=", "<<", ">>", "->", "::", ".*", "**", "//",
}

func matchOperator(src []rune, i int) string {
	rest := src[i:]
	for _, op := range operators {
		r := []rune(op)
		if len(rest) < len(r) {
			continue
		}
		match := true
		for j, rr := range r {
			if rest[j] != rr {
				match = false
				break
			}
		}
		if match {
			return op
		}
	}
	return ""
}

// skipString walks past a quoted literal, honouring backslash escapes and
// stopping at a newline so that an unterminated quote cannot swallow the rest
// of the file.
func skipString(src []rune, i int) int {
	quote := src[i]
	i++
	for i < len(src) {
		switch src[i] {
		case '\\':
			i += 2
			continue
		case quote:
			return i + 1
		case '\n':
			return i
		}
		i++
	}
	return i
}

func isDigit(r rune) bool      { return r >= '0' && r <= '9' }
func isIdentStart(r rune) bool { return r == '_' || r > 127 || isLetter(r) }
func isIdentRune(r rune) bool  { return isIdentStart(r) || isDigit(r) }
func isLetter(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
}
