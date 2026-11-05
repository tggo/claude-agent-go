package eval

import (
	"regexp"
	"strings"
	"unicode"
)

// tokenize lowercases s and splits it into words on anything that is not a
// letter, digit or combining mark, so it works for any script (Ukrainian
// included). Apostrophes inside a word ("м'ясо", "don't") are kept.
func tokenize(s string) []string {
	isApos := func(r rune) bool { return r == '\'' || r == '’' || r == 'ʼ' }
	words := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r) && !unicode.IsMark(r) && !isApos(r)
	})
	out := words[:0]
	for _, w := range words {
		if w = strings.TrimFunc(w, isApos); w != "" {
			out = append(out, w)
		}
	}
	return out
}

func fMeasure(overlap, cand, ref int) float64 {
	if overlap == 0 || cand == 0 || ref == 0 {
		return 0
	}
	p := float64(overlap) / float64(cand)
	r := float64(overlap) / float64(ref)
	return 2 * p * r / (p + r)
}

// emptyCase handles the degenerate inputs shared by the ROUGE functions: two
// empty texts are identical (1), one empty text shares nothing (0).
func emptyCase(c, r []string) (float64, bool) {
	switch {
	case len(c) == 0 && len(r) == 0:
		return 1, true
	case len(c) == 0 || len(r) == 0:
		return 0, true
	}
	return 0, false
}

// Rouge1 is the unigram ROUGE F-measure of candidate against reference, in 0..1.
func Rouge1(candidate, reference string) float64 {
	c, r := tokenize(candidate), tokenize(reference)
	if v, ok := emptyCase(c, r); ok {
		return v
	}
	counts := map[string]int{}
	for _, w := range r {
		counts[w]++
	}
	overlap := 0
	for _, w := range c {
		if counts[w] > 0 {
			counts[w]--
			overlap++
		}
	}
	return fMeasure(overlap, len(c), len(r))
}

// RougeL is the ROUGE-L F-measure (longest common subsequence of tokens) of
// candidate against reference, in 0..1.
func RougeL(candidate, reference string) float64 {
	c, r := tokenize(candidate), tokenize(reference)
	if v, ok := emptyCase(c, r); ok {
		return v
	}
	prev := make([]int, len(r)+1)
	cur := make([]int, len(r)+1)
	for i := 1; i <= len(c); i++ {
		for j := 1; j <= len(r); j++ {
			switch {
			case c[i-1] == r[j-1]:
				cur[j] = prev[j-1] + 1
			case prev[j] >= cur[j-1]:
				cur[j] = prev[j]
			default:
				cur[j] = cur[j-1]
			}
		}
		prev, cur = cur, prev
	}
	return fMeasure(prev[len(r)], len(c), len(r))
}

// Contains reports whether response contains every one of subs,
// case-insensitively.
func Contains(response string, subs ...string) bool {
	low := strings.ToLower(response)
	for _, s := range subs {
		if !strings.Contains(low, strings.ToLower(s)) {
			return false
		}
	}
	return true
}

// Matches reports whether response matches the regular expression pattern
// (RE2 syntax; use (?i) for case-insensitivity). An invalid pattern is an error.
func Matches(response, pattern string) (bool, error) {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return false, err
	}
	return re.MatchString(response), nil
}
