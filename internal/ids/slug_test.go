package ids

import (
	"crypto/sha256"
	"encoding/hex"
	"math"
	"regexp"
	"strings"
	"testing"
)

// The documented formats. ValidSlug must agree with exactly these.
var (
	wordRE   = regexp.MustCompile(`^[a-z]{3,9}(-[a-z]{3,9}){3}$`)
	legacyRE = regexp.MustCompile(`^[a-z2-7]{26}$`)
)

// TestWordlistPinned pins the embedded EFF file and the derived vocabulary.
// DESIGN.md records the same count and entropy; update both together.
func TestWordlistPinned(t *testing.T) {
	sum := sha256.Sum256(effLargeWordlist)
	if got, want := hex.EncodeToString(sum[:]), "addd35536511597a02fa0a9ff1e5284677b8883b83e986e43f15a3db996b903e"; got != want {
		t.Fatalf("embedded wordlist sha256 = %s, want %s (the unmodified EFF large wordlist)", got, want)
	}
	if got := strings.Count(string(effLargeWordlist), "\n"); got != 7776 {
		t.Fatalf("embedded wordlist has %d lines, want 7776", got)
	}
	if len(words) != 7772 {
		t.Fatalf("vocabulary has %d words, want 7772", len(words))
	}
	vocab := sha256.Sum256([]byte(strings.Join(words, "\n")))
	if got, want := hex.EncodeToString(vocab[:]), "76e3348ad957cad7d9c5c68873e89410ed998c19f4956367ab52d9aec4eee884"; got != want {
		t.Fatalf("vocabulary sha256 = %s, want %s", got, want)
	}
	seen := map[string]bool{}
	for _, w := range words {
		if !regexp.MustCompile(`^[a-z]{3,9}$`).MatchString(w) || seen[w] {
			t.Fatalf("bad or duplicate word %q", w)
		}
		seen[w] = true
	}
	if bits := SlugWords * math.Log2(float64(len(words))); bits < 50 {
		t.Fatalf("slug entropy %.2f bits, want >= 50", bits)
	}
}

func TestLoadWordsFilters(t *testing.T) {
	raw := []byte("11111\tabacus\n11112\tyo-yo\n11113\tab\n11114\tabacus\n11115\tEcho\n11116\tdinosaurs\n11121\tcrocodiles\n")
	got := strings.Join(loadWords(raw), ",")
	if got != "abacus,dinosaurs" {
		t.Fatalf("loadWords = %q", got)
	}
}

func TestSlugFormat(t *testing.T) {
	for range 2000 {
		s := Slug()
		if !wordRE.MatchString(s) || !ValidSlug(s) {
			t.Fatalf("Slug() = %q does not match the word format", s)
		}
		if len(s) < 15 || len(s) > MaxSlugLen || MaxSlugLen > 63 {
			t.Fatalf("Slug() = %q has length %d outside [15, %d]", s, len(s), MaxSlugLen)
		}
	}
}

// TestSlugUniformity is a sanity check, not a proof: over many draws each
// word position should hit every bucket of the vocabulary about equally.
func TestSlugUniformity(t *testing.T) {
	index := make(map[string]int, len(words))
	for i, w := range words {
		index[w] = i
	}
	const buckets, draws = 16, 20000
	var counts [SlugWords][buckets]int
	for range draws {
		for pos, w := range strings.Split(Slug(), "-") {
			counts[pos][index[w]*buckets/len(words)]++
		}
	}
	// Expected 1250 per bucket, sd ~34; 6 sd keeps flakes negligible.
	for pos := range counts {
		for b, n := range counts[pos] {
			if math.Abs(float64(n)-draws/buckets) > 6*math.Sqrt(draws/buckets) {
				t.Errorf("position %d bucket %d: %d draws, want ~%d", pos, b, n, draws/buckets)
			}
		}
	}
}

func TestValidSlug(t *testing.T) {
	valid := []string{
		"amber-otter-quilt-river",
		"abc-abc-abc-abc",
		"abcdefghi-abcdefghi-abcdefghi-abcdefghi",
		"s3eybdkc67r7zegmb7qbugpmvy",
		"abcdefghijklmnopqrstuvwxyz",
		"22222222222222222222222222",
	}
	invalid := []string{
		"",
		"amber-otter-quilt",             // 3 words
		"amber-otter-quilt-river-stone", // 5 words
		"Amber-otter-quilt-river",       // uppercase
		"amber-otter-quilt-RIVER",       // uppercase
		"amber-0tter-quilt-river",       // digit
		"amber-otter-quilt-river2",      // digit
		"ab-otter-quilt-river",          // word too short
		"amber-otter-quilt-abcdefghij",  // word too long
		"amber--otter-quilt-river",      // empty word
		"-amber-otter-quilt-river",      // leading dash
		"amber-otter-quilt-river-",      // trailing dash
		"amber_otter_quilt_river",       // wrong divider
		"amber.otter.quilt.river",       // dots
		"amber-otter-quilt-rivér",       // non-ASCII
		"s3eybdkc67r7zegmb7qbugpmv",     // 25 chars
		"s3eybdkc67r7zegmb7qbugpmvyx",   // 27 chars
		"S3EYBDKC67R7ZEGMB7QBUGPMVY",    // uppercase legacy
		"s3eybdkc67r7zegmb7qbugpm0y",    // 0 is not base32
		"s3eybdkc67r7zegmb7qbugpm8y",    // 8 is not base32
		"s3eybdkc67r7zegmb7qbugpm-y",    // dash in legacy
		"s3eybdkc67r7zegmb7qbugpmvy\n",  // trailing newline
		" amber-otter-quilt-river",      // leading space
	}
	for _, s := range valid {
		if !ValidSlug(s) {
			t.Errorf("ValidSlug(%q) = false, want true", s)
		}
	}
	for _, s := range invalid {
		if ValidSlug(s) {
			t.Errorf("ValidSlug(%q) = true, want false", s)
		}
	}
	// ValidSlug agrees with the documented regexes on every case above.
	for _, s := range append(valid, invalid...) {
		if want := wordRE.MatchString(s) || legacyRE.MatchString(s); ValidSlug(s) != want {
			t.Errorf("ValidSlug(%q) = %v, regex says %v", s, !want, want)
		}
	}
}

func FuzzValidSlug(f *testing.F) {
	for _, s := range []string{"amber-otter-quilt-river", "s3eybdkc67r7zegmb7qbugpmvy", "a-b-c-d", "amber-otter-quilt-river\n"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if want := wordRE.MatchString(s) || legacyRE.MatchString(s); ValidSlug(s) != want {
			t.Fatalf("ValidSlug(%q) = %v, regex says %v", s, !want, want)
		}
	})
}
