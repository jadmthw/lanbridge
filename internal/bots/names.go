package bots

import "strings"

// AI player names show up in everyone's chat, so slurs are refused. Names
// are normalized first (case, digits used as letters) so simple disguises
// still match, while ordinary names that merely contain the same letters
// (Knight, Raccoon, Spicy) are left alone.

// substrings are distinctive enough to refuse anywhere in a name. They're
// matched with repeated letters collapsed ("niiigga" -> "niga").
var slurSubstrings = []string{"niga", "niger", "nigr", "fagot", "wetback", "raghead", "towelhead", "trany", "retard"}

// slurWords are refused only as a whole word in the name (split on _ and digits).
var slurWords = map[string]bool{"fag": true, "fags": true, "kike": true, "kyke": true, "spic": true, "spick": true, "gook": true,
	"coon": true, "paki": true, "dyke": true, "chink": true, "beaner": true, "tranny": true}

var leet = strings.NewReplacer("0", "o", "1", "i", "3", "e", "4", "a", "5", "s", "7", "t", "8", "b", "9", "g", "!", "i", "$", "s", "@", "a")

func collapse(s string) string {
	var b strings.Builder
	var last rune
	for _, r := range s {
		if r != last {
			b.WriteRune(r)
		}
		last = r
	}
	return b.String()
}

func offensiveName(name string) bool {
	lower := strings.ToLower(name)
	letters := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' {
			return r
		}
		return -1
	}, leet.Replace(lower))
	flat := collapse(letters)
	for _, s := range slurSubstrings {
		if i := strings.Index(flat, s); i >= 0 && !(s == "niger" && strings.HasPrefix(flat[i:], "nigeria")) {
			return true
		}
	}
	// whole words: split where the original name has underscores or digits
	for _, w := range strings.FieldsFunc(lower, func(r rune) bool { return r == '_' || (r >= '0' && r <= '9') }) {
		if slurWords[w] || slurWords[leet.Replace(w)] {
			return true
		}
	}
	return false
}
