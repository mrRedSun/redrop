package ids

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestParseHostLabel(t *testing.T) {
	word, legacy := "amber-otter-quilt-river", "abcdefghijklmnopqrstuvwxyz"
	ok := map[string]Host{
		word:                        {slug: word},
		word + "--raw":              {slug: word, raw: true},
		word + "--v1":               {slug: word, raw: true, version: 1},
		word + "--v999999":          {slug: word, raw: true, version: 999999},
		legacy:                      {slug: legacy},
		legacy + "--raw":            {slug: legacy, raw: true},
		legacy + "--v42":            {slug: legacy, raw: true, version: 42},
		"abc-def-ghi-jklmnopqr--v7": {slug: "abc-def-ghi-jklmnopqr", raw: true, version: 7},
	}
	for label, want := range ok {
		got, valid := ParseHostLabel(label)
		if !valid || got != want {
			t.Errorf("ParseHostLabel(%q) = %+v, %v; want %+v", label, got, valid, want)
			continue
		}
		if got.Label() != label || len(label) > 63 {
			t.Errorf("Label() = %q, want %q (≤63)", got.Label(), label)
		}
	}
	misses := []string{
		"", "--raw", "--v1", word + "-raw", word + "---raw", word + "--raw--raw", word + "--rawx",
		word + "--RAW", word + "--v", word + "--v0", word + "--v01", word + "--v1234567", word + "--v1x",
		word + "--v-1", word + "--v+1", word + "--x", word + "--", "amber-otter-quilt--raw",
		"Amber-otter-quilt-river--raw", legacy + "x--raw", legacy[:25] + "--raw", word + ".x--raw",
	}
	for _, label := range misses {
		if h, valid := ParseHostLabel(label); valid {
			t.Errorf("ParseHostLabel(%q) accepted: %+v", label, h)
		}
	}
	longest := RawHost("abcdefghi-abcdefghi-abcdefghi-abcdefghi", MaxHostVersion).Label()
	if len(longest) != 48 {
		t.Errorf("longest label %q has %d octets", longest, len(longest))
	}
}

// TestReadmeProxyRule: the reverse-proxy regex in the README accepts exactly
// the labels ParseHostLabel accepts (for these cases), so following the
// README never leaves a valid host unrouted.
func TestReadmeProxyRule(t *testing.T) {
	readme, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile("(?m)^(\\^\\(\\[a-z2-7\\].*)\\\\.usercontent\\\\.example\\$$").FindSubmatch(readme)
	if m == nil {
		t.Fatal("README proxy regex not found")
	}
	rule := regexp.MustCompile(string(m[1]) + "$")
	word, legacy := "amber-otter-quilt-river", "abcdefghijklmnopqrstuvwxyz"
	for _, label := range []string{word, word + "--raw", word + "--v1", word + "--v999999", legacy, legacy + "--raw", legacy + "--v42",
		word + "--v0", word + "--v01", word + "--v1234567", word + "--rawx", word + "---raw", "amber-otter-quilt--raw", strings.ToUpper(word)} {
		_, parsed := ParseHostLabel(label)
		if rule.MatchString(label) != parsed {
			t.Errorf("label %q: README rule %v, parser %v", label, rule.MatchString(label), parsed)
		}
	}
}
