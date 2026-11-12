package fold

import (
	"strings"
	"unicode"
)

// Vietnamese letters are precomposed, not base+combining marks, so stripping unicode.Mn
// alone leaves "lương" untouched. The table below is the whole alphabet that matters.
var foldTable = map[rune]rune{}

func init() {
	groups := map[rune]string{
		'a': "àáảãạăằắẳẵặâầấẩẫậ",
		'e': "èéẻẽẹêềếểễệ",
		'i': "ìíỉĩị",
		'o': "òóỏõọôồốổỗộơờớởỡợ",
		'u': "ùúủũụưừứửữự",
		'y': "ỳýỷỹỵ",
		'd': "đ",
	}
	for base, variants := range groups {
		foldTable[base] = base
		for _, v := range variants {
			foldTable[v] = base
		}
	}
}

// Fold lowercases and drops diacritics: "lương" == "luong" == "LƯƠNG".
// People type without tones on a phone keyboard; matching only the accented spelling means
// answering "no tool for this" to a question the system understood perfectly.
func Fold(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		r = unicode.ToLower(r)
		if base, ok := foldTable[r]; ok {
			b.WriteRune(base)
			continue
		}
		if unicode.Is(unicode.Mn, r) {
			continue
		}
		b.WriteRune(r)
	}
	return strings.Join(strings.Fields(b.String()), " ")
}
