package eval

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestTokenize(t *testing.T) {
	assert.Equal(t, []string{"hello", "world", "42"}, tokenize("Hello, WORLD! 42."))
	assert.Equal(t, []string{"м'ясо", "дуже", "смачне"}, tokenize("М'ясо — дуже СМАЧНЕ!"))
	assert.Equal(t, []string{"don't", "stop"}, tokenize("'don't' stop"))
	assert.Equal(t, []string{"\u0438\u0306"}, tokenize("\u0418\u0306"), "combining marks stay in the word")
	assert.Empty(t, tokenize("...  "))
}

func TestRouge(t *testing.T) {
	tests := []struct {
		name         string
		cand, ref    string
		wantL, want1 float64
	}{
		{"identical", "the cat sat on the mat", "The cat sat on the mat.", 1, 1},
		{"both empty", "", "  ", 1, 1},
		{"candidate empty", "", "cat", 0, 0},
		{"reference empty", "cat", "", 0, 0},
		{"disjoint", "dog runs", "cat sits", 0, 0},
		// LCS "the cat" = 2 of 3 tokens each way -> 2/3.
		{"partial", "the cat ran", "the cat sat", 2.0 / 3, 2.0 / 3},
		// same bag of words, reversed order: rouge1 = 1, LCS = 1 of 3.
		{"order matters for L", "c b a", "a b c", 1.0 / 3, 1},
		// clipping: candidate repeats "a" three times, ref has it once.
		// overlap 1, p = 1/3, r = 1 -> f = 0.5
		{"clipped counts", "a a a", "a", 0.5, 0.5},
		{"ukrainian identical", "Файл успішно збережено", "файл успішно збережено!", 1, 1},
		{"ukrainian partial", "Файл успішно збережено у теці", "файл збережено", 2 * (2.0 / 5) * 1 / (2.0/5 + 1), 2 * (2.0 / 5) * 1 / (2.0/5 + 1)},
		{"ukrainian disjoint", "привіт світе", "бувай місяцю", 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.InDelta(t, tt.wantL, RougeL(tt.cand, tt.ref), 1e-9, "RougeL")
			assert.InDelta(t, tt.want1, Rouge1(tt.cand, tt.ref), 1e-9, "Rouge1")
		})
	}
}

func TestContainsAndMatches(t *testing.T) {
	assert.True(t, Contains("Файл Збережено OK", "збережено", "ok"))
	assert.False(t, Contains("abc", "abc", "d"))
	assert.True(t, Contains("anything"))

	ok, err := Matches("exit code 0", `exit code \d+`)
	assert.NoError(t, err)
	assert.True(t, ok)
	ok, err = Matches("nope", `^\d+$`)
	assert.NoError(t, err)
	assert.False(t, ok)
	_, err = Matches("x", `(`)
	assert.Error(t, err)
}
