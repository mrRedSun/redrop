package markdown

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"net/url"
	"strings"
	"testing"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

func renderDoc(t *testing.T, source string) string {
	t.Helper()
	out, err := Render([]byte(source), "notes.md", "")
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func parseDoc(t *testing.T, source string) *html.Node {
	t.Helper()
	n, err := html.Parse(strings.NewReader(source))
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestFeatures(t *testing.T) {
	doc := renderDoc(t, "# Field notes\n\n## Setup\n\n```go\npackage main\nfunc main() {}\n```\n\n| A | B |\n|---|---|\n| x | y |\n\n- [x] Done\n- [ ] Next\n\n> Quote\n\n~~old~~ **bold** *em*\n\n![Sketch](images/sketch.png)\n")
	for _, want := range []string{
		"<title>Field notes</title>", `id="md-field-notes"`, `href="#md-setup"`,
		`<span class="kn">package</span>`, "<table>", `type="checkbox"`, `disabled=""`, `checked=""`,
		"<blockquote>", "<del>old</del>", "<strong>bold</strong>", "<em>em</em>",
		`src="./images/sketch.png"`, "prefers-color-scheme:dark", "Copy code", `href="notes.md?download" download`,
		`connect-src 'none'`,
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(doc, `aria-label="On this page"`) {
		t.Error("short document should not have a TOC")
	}
}

func TestTOCAndHeadingIDs(t *testing.T) {
	doc := renderDoc(t, "# Test\n\n## Same\n\n### Same\n\n## Same-2\n\n## 中文 & <unsafe>\n")
	for _, want := range []string{`class="toc"`, `class="mobile-toc"`, `id="md-same"`, `id="md-same-2"`, `id="md-same-2-2"`, `class="level-3"`, `id="md-中文--"`} {
		if !strings.Contains(doc, want) {
			t.Errorf("missing %q", want)
		}
	}
	root := parseDoc(t, doc)
	ids := map[string]bool{}
	walk(root, func(n *html.Node) {
		for _, a := range n.Attr {
			if a.Key == "id" {
				if ids[a.Val] {
					t.Errorf("duplicate id %q", a.Val)
				}
				ids[a.Val] = true
			}
		}
	})
	walk(root, func(n *html.Node) {
		for _, a := range n.Attr {
			if a.Key == "href" && strings.HasPrefix(a.Val, "#") {
				id, err := url.PathUnescape(strings.TrimPrefix(a.Val, "#"))
				if err != nil || !ids[id] {
					t.Errorf("broken anchor %q", a.Val)
				}
			}
		}
	})
}

func TestAuthoredHeadingLinks(t *testing.T) {
	doc := renderDoc(t, "# Overview\n\n[Jump to setup](#setup) [Permalink](#md-setup) [Main](#main) [Unicode](#%E4%B8%AD%E6%96%87) [Second](#setup-2)\n\n## Setup\n\n## Setup\n\n## 中文\n")
	for _, want := range []string{`href="#md-setup">Jump to setup</a>`, `href="#md-setup">Permalink</a>`, `href="#main">Main</a>`, `href="#md-中文">Unicode</a>`, `href="#md-setup-2">Second</a>`} {
		if !strings.Contains(doc, want) {
			t.Errorf("missing working authored heading link %s", want)
		}
	}
}

func TestDuplicateHeadingsLinearAllocations(t *testing.T) {
	allocs := func(count int) float64 {
		source := []byte(strings.Repeat("# A\n", count))
		return testing.AllocsPerRun(1, func() {
			if _, err := Render(source, "notes.md", ""); err != nil {
				t.Fatal(err)
			}
		})
	}
	small, large := allocs(100), allocs(1000)
	// Ten times as many headings should take roughly ten times the allocations,
	// not the hundredfold growth caused by restarting duplicate suffix probes.
	if large > small*20 {
		t.Fatalf("duplicate-heading allocations grow superlinearly: 100 headings %.0f, 1000 headings %.0f", small, large)
	}
}

func TestXSSCorpus(t *testing.T) {
	corpus := []string{
		`<script>alert(1)</script>`,
		`<img src=x onerror="alert(1)">`,
		`<svg onload=alert(1)><foreignObject><iframe srcdoc="<script>alert(1)</script>"></iframe></foreignObject></svg>`,
		`[x](javascript:alert%281%29)`,
		`[x](JaVaScRiPt:alert%281%29)`,
		`[x](java&#x73;cript:alert%281%29)`,
		`[x](java&#x09;script:alert%281%29)`,
		`[x](data:text/html;base64,PHNjcmlwdD4=)`,
		`![x](data:image/svg+xml;base64,PHN2ZyBvbmxvYWQ9YWxlcnQoMSk+)`,
		`<!-- <img src=x onerror=alert(1)> -->`,
		`<javascript:alert(1)>`,
		`<https://example.com/track>`,
		`https://example.com/track`,
		`![pixel](https://example.com/track)`,
		`![pixel](//example.com/track)`,
		`[x](\\example.com/track)`,
		`![x](%2f%2fexample.com/track)`,
		`[x](/__redrop/open)`,
		`[x](__redrop/open)`,
		`[x](../escape)`,
		`[x](file:///etc/passwd)`,
		`[x](vbscript:msgbox%281%29)`,
		`<base href="https://example.com/"><meta http-equiv="refresh" content="0;url=https://example.com/">`,
		"# </title><script>alert(1)</script>\n\n```html\n</code><script>alert(1)</script>\n```",
	}
	for _, payload := range corpus {
		t.Run(payload, func(t *testing.T) {
			root := parseDoc(t, renderDoc(t, payload))
			var main *html.Node
			walk(root, func(n *html.Node) {
				if n.Data == "main" {
					main = n
				}
			})
			if main == nil {
				t.Fatal("missing document body")
			}
			walk(main, func(n *html.Node) {
				if n.Type == html.CommentNode {
					t.Error("HTML comment survived")
				}
				if n.Type == html.ElementNode && (n.Data == "script" || n.Data == "iframe" || n.Data == "svg" || n.Data == "style" || n.Data == "base" || n.Data == "meta") {
					t.Errorf("active element %s survived", n.Data)
				}
				for _, a := range n.Attr {
					if strings.HasPrefix(a.Key, "on") || a.Key == "style" || a.Key == "srcdoc" {
						t.Errorf("active attribute %s survived", a.Key)
					}
					if a.Key == "href" || a.Key == "src" {
						if v, ok := localURL(a.Val, ""); !ok || v == "" && a.Val != "" {
							t.Errorf("unsafe URL %q survived", a.Val)
						}
					}
				}
			})
		})
	}
}

func TestSanitizerAllowlist(t *testing.T) {
	// Exercise the allowlist independently of Goldmark's raw-HTML protection.
	input := `<p onclick="x()">ok<script>x()</script><!-- secret --><img src="javascript:x()" onerror="x()"><a href="https://example.com/">remote</a><iframe srcdoc="x"></iframe><input type="submit" autofocus formaction="https://example.com/"><span class="k" style="background:url(https://example.com/)">code</span></p>`
	nodes, err := html.ParseFragment(strings.NewReader(input), &html.Node{Type: html.ElementNode, Data: "div", DataAtom: atom.Div})
	if err != nil {
		t.Fatal(err)
	}
	root := &html.Node{Type: html.ElementNode, Data: "div"}
	for _, n := range nodes {
		root.AppendChild(n)
	}
	sanitize(root, "")
	var out bytes.Buffer
	if err := html.Render(&out, root); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"script", "onclick", "onerror", "javascript", "https:", "iframe", "submit", "autofocus", "formaction", "style=", "<!--"} {
		if strings.Contains(out.String(), bad) {
			t.Errorf("sanitizer retained %q: %s", bad, out.String())
		}
	}
}

func TestLocalURLs(t *testing.T) {
	for _, bad := range []string{"javascript:x", "data:text/html,x", "https://example.com", "//example.com", `/root`, `%2f%2fexample.com`, `a%5cb`, "a\nb", "a%00b", "../../escape", "../__redrop/open", "../%5cevil"} {
		if got, ok := localURL(bad, "docs"); ok {
			t.Errorf("accepted %q as %q", bad, got)
		}
	}
	for input, want := range map[string]string{"images/plot.png": "./docs/images/plot.png", "../assets/plot.png": "./assets/plot.png", "#section": "#section", "notes.md?download#one": "./docs/notes.md?download#one", "my%20image.png": "./docs/my%20image.png"} {
		if got, ok := localURL(input, "docs"); !ok || got != want {
			t.Errorf("localURL(%q) = %q, %v; want %q", input, got, ok, want)
		}
	}
}

func TestCSPAndDeterminism(t *testing.T) {
	doc := renderDoc(t, "# <img src=x onerror=alert(1)> & Title\n")
	if doc != renderDoc(t, "# <img src=x onerror=alert(1)> & Title\n") {
		t.Error("render is not deterministic")
	}
	root := parseDoc(t, doc)
	var script, csp string
	count := 0
	walk(root, func(n *html.Node) {
		if n.Data == "script" {
			script = nodeText(n)
			count++
		}
		if n.Data == "meta" {
			for _, a := range n.Attr {
				if a.Key == "content" && strings.Contains(a.Val, "script-src") {
					csp = a.Val
				}
			}
		}
	})
	sum := sha256.Sum256([]byte(script))
	if count != 1 || !strings.Contains(csp, "script-src 'sha256-"+base64.StdEncoding.EncodeToString(sum[:])+"'") {
		t.Fatalf("CSP does not pin the sole trusted script: count %d, CSP %q", count, csp)
	}
}

func TestLimitsAndPlainCode(t *testing.T) {
	if _, err := Render(make([]byte, MaxSourceBytes+1), "large.md", ""); err == nil {
		t.Error("oversize source accepted")
	}
	for _, source := range []string{"```unknown\n<script>alert(1)</script>\n```", "    <script>alert(1)</script>\n", "```go\n" + strings.Repeat("x", 65<<10) + "\n```"} {
		doc := renderDoc(t, source)
		if !strings.Contains(doc, "<pre><code>") || strings.Contains(doc, "<script>alert") {
			t.Error("plain code should be escaped and remain copyable")
		}
	}
}
