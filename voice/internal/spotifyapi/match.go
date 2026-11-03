package spotifyapi

import (
	"strings"
	"unicode"
)

// The model hears names by ear and often spells a foreign one in Russian
// letters, so names are compared by sound: both sides in Latin letters.

var translit = map[rune]string{
	'а': "a", 'б': "b", 'в': "v", 'г': "g", 'д': "d", 'е': "e", 'ё': "e", 'ж': "zh", 'з': "z", 'и': "i", 'й': "y",
	'к': "k", 'л': "l", 'м': "m", 'н': "n", 'о': "o", 'п': "p", 'р': "r", 'с': "s", 'т': "t", 'у': "u", 'ф': "f",
	'х': "h", 'ц': "ts", 'ч': "ch", 'ш': "sh", 'щ': "sch", 'ъ': "", 'ы': "y", 'ь': "", 'э': "e", 'ю': "yu", 'я': "ya",
}

// toLatin spells Russian letters in Latin ones and keeps everything else.
func toLatin(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if t, ok := translit[r]; ok {
			b.WriteString(t)
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// hasCyrillic reports whether s has a Russian letter in it.
func hasCyrillic(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool { return unicode.Is(unicode.Cyrillic, r) })
}

// normName is s for comparing: Latin letters and digits, single spaces.
func normName(s string) string {
	s = toLatin(s)
	return strings.Join(strings.FieldsFunc(s, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }), " ")
}

// bigrams counts the letter pairs of s, the spaces left out. What transliteration
// cannot keep is evened out first: y and i, doubled letters.
func bigrams(s string) map[string]int {
	var r []rune
	for _, c := range strings.ReplaceAll(strings.ReplaceAll(s, " ", ""), "y", "i") {
		if len(r) == 0 || r[len(r)-1] != c {
			r = append(r, c)
		}
	}
	m := make(map[string]int, len(r))
	for i := 0; i+1 < len(r); i++ {
		m[string(r[i:i+2])]++
	}
	return m
}

// similar is how alike a and b sound, 0..1: the Dice coefficient of their
// letter pairs, so word order and a misheard letter cost little.
func similar(a, b string) float64 {
	a, b = normName(a), normName(b)
	if a == "" || b == "" {
		return 0
	}
	if a == b {
		return 1
	}
	x, y := bigrams(a), bigrams(b)
	shared, total := 0, 0
	for k, n := range x {
		shared += min(n, y[k])
		total += n
	}
	for _, n := range y {
		total += n
	}
	if total == 0 {
		return 0
	}
	return 2 * float64(shared) / float64(total)
}
