package ids

import (
	"bufio"
	"bytes"
	"crypto/rand"
	_ "embed"
	"math/big"
	"strings"
)

// Artifact slugs come in two formats. Both are accepted forever; only the
// word format is minted.
//
//   - Word (current):   four words from the EFF large wordlist joined by "-",
//     e.g. "amber-otter-quilt-river" (^[a-z]{3,9}(-[a-z]{3,9}){3}$, ~51.7 bits).
//   - Legacy (before word slugs): 26 lowercase base32 characters
//     (^[a-z2-7]{26}$, 130 bits).
const (
	SlugWords       = 4
	legacySlugLen   = 26
	minWordLen      = 3
	maxWordLen      = 9
	MaxSlugLen      = SlugWords*maxWordLen + SlugWords - 1 // 39, within a 63-octet DNS label
	legacyAlphabet  = "abcdefghijklmnopqrstuvwxyz234567"
	slugWordDivider = '-'
)

// effLargeWordlist is the unmodified EFF large diceware list (CC BY 3.0 US,
// see NOTICE): 7776 lines of "<dice digits>\t<word>".
//
//go:embed eff_large_wordlist.txt
var effLargeWordlist []byte

// words is the slug vocabulary: EFF entries matching ^[a-z]{3,9}$, deduplicated,
// in list order.
var words = loadWords(effLargeWordlist)

func loadWords(raw []byte) []string {
	seen := map[string]bool{}
	var out []string
	sc := bufio.NewScanner(bytes.NewReader(raw))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) != 2 || !isWord(f[1]) || seen[f[1]] {
			continue
		}
		seen[f[1]] = true
		out = append(out, f[1])
	}
	return out
}

func isWord(w string) bool {
	if len(w) < minWordLen || len(w) > maxWordLen {
		return false
	}
	for i := 0; i < len(w); i++ {
		if w[i] < 'a' || w[i] > 'z' {
			return false
		}
	}
	return true
}

// Slug mints a new secret artifact subdomain: SlugWords uniformly chosen
// words. crypto/rand.Int uses rejection sampling, so there is no modulo bias.
func Slug() string { return slugFrom(rand.Reader) }

func slugFrom(r interface{ Read([]byte) (int, error) }) string {
	n := big.NewInt(int64(len(words)))
	parts := make([]string, SlugWords)
	for i := range parts {
		k, err := rand.Int(r, n)
		if err != nil {
			panic(err)
		}
		parts[i] = words[k.Int64()]
	}
	return strings.Join(parts, string(slugWordDivider))
}

// ValidSlug reports whether s is a well-formed artifact slug in either the
// word or the legacy format. It is the only slug syntax check in redrop: use
// it before every lookup of a slug taken from a request, URL, or argument.
func ValidSlug(s string) bool { return validWordSlug(s) || validLegacySlug(s) }

func validLegacySlug(s string) bool {
	if len(s) != legacySlugLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		if strings.IndexByte(legacyAlphabet, s[i]) < 0 {
			return false
		}
	}
	return true
}

func validWordSlug(s string) bool {
	if len(s) > MaxSlugLen {
		return false
	}
	parts := strings.Split(s, string(slugWordDivider))
	if len(parts) != SlugWords {
		return false
	}
	for _, p := range parts {
		if !isWord(p) {
			return false
		}
	}
	return true
}
