package sharing

import (
	"strings"
	"testing"

	"github.com/mrRedSun/redrop/internal/apperr"
	"github.com/mrRedSun/redrop/internal/config"
	"github.com/mrRedSun/redrop/internal/store"
)

func TestFeedbackOpen(t *testing.T) {
	on, off := true, false
	s := &Service{cfg: &config.Config{InfoButtonDefault: true}}
	for _, tc := range []struct {
		name     string
		a        store.Artifact
		want     bool
		defaultI bool
	}{
		{"off by default", store.Artifact{}, false, true},
		{"on, info by default", store.Artifact{Feedback: true}, true, true},
		{"on, info on", store.Artifact{Feedback: true, InfoButton: &on}, true, false},
		{"on, info off", store.Artifact{Feedback: true, InfoButton: &off}, false, true},
		{"on, info default off", store.Artifact{Feedback: true}, false, false},
	} {
		s.cfg.InfoButtonDefault = tc.defaultI
		if got := s.FeedbackOpen(&tc.a); got != tc.want {
			t.Errorf("%s: open = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestValidComment(t *testing.T) {
	a := &store.Artifact{ID: "a1", CurrentVersion: 2, LastVersion: 3}
	ok := FeedbackPost{Body: "Fix the chart", Version: 2, Page: "/report.html?x=1#totals"}
	if c, err := validComment(a, ok); err != nil || c.Body != "Fix the chart" || c.Page != ok.Page || c.Version != 2 || c.ArtifactID != "a1" {
		t.Fatalf("valid post: %+v %v", c, err)
	}

	// Normalization: control characters go, line breaks and tabs stay in
	// multiline fields, the name becomes one line, and outer space is trimmed.
	c, err := validComment(a, FeedbackPost{
		Body: "  line one\r\nline\ttwo\x1b[31m red\x00\u0085\x7f  ", Name: " Sam\nthe\tcritic\u009b ", Version: 3, Page: "/",
		Quote: &TextQuote{Exact: "a\r\nb\x07", Prefix: "pre\x1b", Suffix: "post"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if c.Body != "line one\nline\ttwo[31m red" || c.Name != "Samthecritic" || c.QuoteExact != "a\nb" || c.QuotePrefix != "pre" || c.QuoteSuffix != "post" {
		t.Fatalf("normalized: body %q name %q quote %q/%q/%q", c.Body, c.Name, c.QuoteExact, c.QuotePrefix, c.QuoteSuffix)
	}

	at := func(n int) string { return strings.Repeat("é", n) } // one code point, two bytes
	valid := []FeedbackPost{
		{Body: at(4000), Version: 1, Page: "/"},
		{Body: "x", Name: at(80), Version: 3, Page: "/"},
		{Body: "x", Version: 1, Page: "/" + strings.Repeat("a", 2047)},
		{Body: "x", Version: 1, Page: "/", Quote: &TextQuote{Exact: at(500), Prefix: at(64), Suffix: at(64)}},
		{Body: "x", Version: 1, Page: "/", Quote: &TextQuote{}},
		{Body: "x", Version: 1, Page: "/redrop-notes/__redrop"},
	}
	for i, p := range valid {
		if _, err := validComment(a, p); err != nil {
			t.Errorf("valid case %d: %v", i, err)
		}
	}
	invalid := map[string]FeedbackPost{
		"empty body":           {Body: " \n\t ", Version: 1, Page: "/"},
		"control-only body":    {Body: "\x00\x1b", Version: 1, Page: "/"},
		"long body":            {Body: at(4001), Version: 1, Page: "/"},
		"long name":            {Body: "x", Name: at(81), Version: 1, Page: "/"},
		"version zero":         {Body: "x", Version: 0, Page: "/"},
		"version above mark":   {Body: "x", Version: 4, Page: "/"},
		"empty page":           {Body: "x", Version: 1, Page: ""},
		"relative page":        {Body: "x", Version: 1, Page: "x.html"},
		"protocol-relative":    {Body: "x", Version: 1, Page: "//evil.example/"},
		"backslash":            {Body: "x", Version: 1, Page: `/\evil.example`},
		"reserved page":        {Body: "x", Version: 1, Page: "/__redrop/v/1/"},
		"reserved root":        {Body: "x", Version: 1, Page: "/__redrop?x"},
		"control in page":      {Body: "x", Version: 1, Page: "/a\nb"},
		"long page":            {Body: "x", Version: 1, Page: "/" + strings.Repeat("a", 2048)},
		"long quote":           {Body: "x", Version: 1, Page: "/", Quote: &TextQuote{Exact: at(501)}},
		"long prefix":          {Body: "x", Version: 1, Page: "/", Quote: &TextQuote{Exact: "q", Prefix: at(65)}},
		"long suffix":          {Body: "x", Version: 1, Page: "/", Quote: &TextQuote{Exact: "q", Suffix: at(65)}},
		"context without text": {Body: "x", Version: 1, Page: "/", Quote: &TextQuote{Prefix: "p"}},
	}
	for name, p := range invalid {
		if _, err := validComment(a, p); !apperr.Is(err, apperr.KindInvalid) {
			t.Errorf("%s: err %v, want invalid", name, err)
		}
	}
}

// Line separators become line feeds (or go, in one-line fields), and
// invisible characters go, so no consumer that splits on Unicode line
// breaks or reads hidden text gets anything the owner can't see.
func TestCleanTextLineBreaksAndInvisibles(t *testing.T) {
	const hidden = "\U000E0049\U000E0047\U000E004E\U000E007F\u200B\u2060\u2061\u2062\u2063\u2064\uFEFF\u180E"
	in := "a\u2028b\u2029c\u0085d\r\ne\rf" + hidden + "g\u200Dh\u200Ci"
	if got, want := cleanText(in, true), "a\nb\nc\nd\ne\nfg\u200Dh\u200Ci"; got != want {
		t.Errorf("multiline: %q, want %q", got, want)
	}
	if got, want := cleanText(in, false), "abcdefg\u200Dh\u200Ci"; got != want {
		t.Errorf("single line: %q, want %q", got, want)
	}
	for _, r := range []rune{0xE0000, 0xE0020, 0xE007F, 0x200B, 0x2060, 0x2064, 0xFEFF, 0x180E} {
		if !invisible(r) {
			t.Errorf("U+%04X kept", r)
		}
	}
	for _, r := range []rune{0x200C, 0x200D, 0xE0080, 0x2065, 'x'} {
		if invisible(r) {
			t.Errorf("U+%04X dropped", r)
		}
	}
	for _, page := range []string{"/a\u2028b", "/a\u2029b", "/a\u0085b", "/a\U000E0041", "/a\u200Bb", "/#\uFEFF"} {
		if validPage(page) {
			t.Errorf("page %q accepted", page)
		}
	}
	if !validPage("/caf\u00E9?q=\u200D#x") {
		t.Error("a page with ordinary Unicode and a joiner was rejected")
	}
}
