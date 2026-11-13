// Package markdown renders untrusted Markdown into an offline document page.
// It is used only by the publishing client, never by the content server.
package markdown

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"fmt"
	"html/template"
	"net/url"
	"path"
	"strconv"
	"strings"
	"unicode"

	"github.com/alecthomas/chroma/v2"
	chromahtml "github.com/alecthomas/chroma/v2/formatters/html"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/alecthomas/chroma/v2/styles"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// MaxSourceBytes bounds parsing memory. Larger files can still be shared raw.
const MaxSourceBytes = 8 << 20

//go:embed page.html
var pageHTML string

var page = template.Must(template.New("document").Parse(pageHTML))
var scriptHash = func() string {
	_, script, _ := strings.Cut(pageHTML, "<script>")
	script, _, _ = strings.Cut(script, "</script>")
	sum := sha256.Sum256([]byte(script))
	return base64.StdEncoding.EncodeToString(sum[:])
}()

type heading struct {
	ID, Text string
	Level    int
}

// Render returns a self-contained HTML page. name and base are slash-separated
// paths inside the bundle. base rebases relative links when the source is nested.
func Render(source []byte, name, base string) ([]byte, error) {
	if len(source) > MaxSourceBytes {
		return nil, fmt.Errorf("Markdown exceeds %d MiB; use --render none to share it raw", MaxSourceBytes>>20)
	}
	var raw bytes.Buffer
	md := goldmark.New(goldmark.WithExtensions(extension.GFM)) // no raw HTML option
	if err := md.Convert(source, &raw); err != nil {
		return nil, fmt.Errorf("render Markdown: %w", err)
	}
	nodes, err := html.ParseFragment(&raw, &html.Node{Type: html.ElementNode, Data: "div", DataAtom: atom.Div})
	if err != nil {
		return nil, fmt.Errorf("parse Markdown output: %w", err)
	}
	root := &html.Node{Type: html.ElementNode, Data: "div"}
	for _, n := range nodes {
		root.AppendChild(n)
	}
	highlight(root)
	sanitize(root, base)
	var headings []heading
	used := map[string]bool{"main": true}
	nextSuffix := map[string]int{}
	fragments := map[string]string{}
	title := path.Base(name)
	foundTitle := false
	walk(root, func(n *html.Node) {
		if n.Type != html.ElementNode || len(n.Data) != 2 || n.Data[0] != 'h' || n.Data[1] < '1' || n.Data[1] > '6' {
			return
		}
		text := nodeText(n)
		if n.Data == "h1" && !foundTitle {
			title, foundTitle = text, true
		}
		slug := "md-" + strings.Map(func(r rune) rune {
			if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_' {
				return unicode.ToLower(r)
			}
			if unicode.IsSpace(r) {
				return '-'
			}
			return -1
		}, text)
		id := slug
		if used[id] {
			i := max(2, nextSuffix[slug])
			for {
				id = slug + "-" + strconv.Itoa(i)
				i++
				if !used[id] {
					break
				}
			}
			nextSuffix[slug] = i
		}
		used[id] = true
		fragments[strings.TrimPrefix(id, "md-")] = id
		n.Attr = append(n.Attr, html.Attribute{Key: "id", Val: id})
		a := &html.Node{Type: html.ElementNode, Data: "a", Attr: []html.Attribute{
			{Key: "class", Val: "anchor"}, {Key: "href", Val: "#" + id}, {Key: "aria-label", Val: "Link to " + text},
		}}
		a.AppendChild(&html.Node{Type: html.TextNode, Data: "#"})
		n.AppendChild(a)
		headings = append(headings, heading{ID: id, Text: text, Level: int(n.Data[1] - '0')})
	})
	// Keep conventional authored links such as #setup working with prefixed IDs.
	// Explicit generated permalinks and template anchors take precedence.
	walk(root, func(n *html.Node) {
		if n.Type != html.ElementNode || n.Data != "a" {
			return
		}
		for i, a := range n.Attr {
			if a.Key != "href" || !strings.HasPrefix(a.Val, "#") {
				continue
			}
			u, err := url.Parse(a.Val)
			if err == nil && !used[u.Fragment] {
				if id, ok := fragments[u.Fragment]; ok {
					n.Attr[i].Val = "#" + id
				}
			}
		}
	})
	var body bytes.Buffer
	for n := root.FirstChild; n != nil; n = n.NextSibling {
		if err := html.Render(&body, n); err != nil {
			return nil, err
		}
	}
	if len(headings) < 4 {
		headings = nil
	}
	var out bytes.Buffer
	err = page.Execute(&out, struct {
		Title, Name, Download, ScriptHash string
		Body                              template.HTML
		Headings                          []heading
	}{title, name, (&url.URL{Path: name, RawQuery: "download"}).String(), scriptHash, template.HTML(body.String()), headings}) // body passed the allowlist above
	return out.Bytes(), err
}

func walk(n *html.Node, fn func(*html.Node)) {
	fn(n)
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		walk(c, fn)
	}
}

func nodeText(n *html.Node) string {
	var s strings.Builder
	walk(n, func(c *html.Node) {
		if c.Type == html.TextNode {
			s.WriteString(c.Data)
		}
	})
	return s.String()
}

func highlight(root *html.Node) {
	walk(root, func(n *html.Node) {
		if n.Data != "pre" || n.FirstChild == nil || n.FirstChild.Data != "code" {
			return
		}
		code := n.FirstChild
		text := nodeText(code)
		// Avoid lexer work on enormous blocks. Unknown languages stay plain text.
		if len(text) > 64<<10 {
			return
		}
		var lexer chroma.Lexer
		for _, a := range code.Attr {
			if a.Key == "class" {
				lexer = lexers.Get(strings.TrimPrefix(a.Val, "language-"))
			}
		}
		if lexer == nil {
			return
		}
		iter, err := lexer.Tokenise(nil, text)
		if err != nil {
			return
		}
		var out bytes.Buffer
		if err := chromahtml.New(chromahtml.WithClasses(true), chromahtml.PreventSurroundingPre(true)).Format(&out, styles.Fallback, iter); err != nil {
			return
		}
		nodes, err := html.ParseFragment(&out, &html.Node{Type: html.ElementNode, Data: "div", DataAtom: atom.Div})
		if err != nil {
			return
		}
		for code.FirstChild != nil {
			code.RemoveChild(code.FirstChild)
		}
		for _, child := range nodes {
			code.AppendChild(child)
		}
	})
}

// Sanitize the generated fragment, including highlighter output. No user HTML,
// event handlers, CSS, embedded documents, or active content reaches the template.
func sanitize(root *html.Node, base string) {
	const allowed = " p h1 h2 h3 h4 h5 h6 a img pre code span strong em del blockquote ul ol li table thead tbody tr th td hr br input "
	for n := root.FirstChild; n != nil; {
		next := n.NextSibling
		if n.Type != html.TextNode && (n.Type != html.ElementNode || !strings.Contains(allowed, " "+n.Data+" ")) {
			root.RemoveChild(n)
			n = next
			continue
		}
		attrs := n.Attr
		n.Attr = nil
		for _, a := range attrs {
			if a.Namespace != "" {
				continue
			}
			switch {
			case n.Data == "a" && a.Key == "href", n.Data == "img" && a.Key == "src":
				if value, ok := localURL(a.Val, base); ok {
					n.Attr = append(n.Attr, html.Attribute{Key: a.Key, Val: value})
				}
			case a.Key == "title" && (n.Data == "a" || n.Data == "img"), a.Key == "alt" && n.Data == "img":
				n.Attr = append(n.Attr, a)
			case n.Data == "span" && a.Key == "class":
				// Chroma's short, fixed token classes only; no styles or URLs.
				if len(a.Val) <= 3 && strings.Trim(a.Val, "abcdefghijklmnopqrstuvwxyz0123456789") == "" {
					n.Attr = append(n.Attr, a)
				}
			case n.Data == "ol" && a.Key == "start":
				if _, err := strconv.Atoi(a.Val); err == nil {
					n.Attr = append(n.Attr, a)
				}
			case n.Data == "input" && a.Key == "checked":
				n.Attr = append(n.Attr, html.Attribute{Key: "checked"})
			}
		}
		if n.Data == "input" {
			n.Attr = append(n.Attr, html.Attribute{Key: "type", Val: "checkbox"}, html.Attribute{Key: "disabled"})
		}
		sanitize(n, base)
		n = next
	}
}

func localURL(value, base string) (string, bool) {
	// Backslashes and controls have browser-specific URL parsing semantics.
	if strings.ContainsAny(value, "\\") || strings.IndexFunc(value, unicode.IsControl) >= 0 {
		return "", false
	}
	u, err := url.Parse(value)
	if err != nil || u.IsAbs() || u.Host != "" || u.User != nil || strings.HasPrefix(value, "//") {
		return "", false
	}
	decoded, err := url.PathUnescape(u.EscapedPath())
	if err != nil || strings.ContainsAny(decoded, "\\") || strings.IndexFunc(decoded, unicode.IsControl) >= 0 {
		return "", false
	}
	if u.Path != "" {
		if strings.HasPrefix(decoded, "/") {
			return "", false
		}
		u.Path = path.Join(base, decoded)
		u.RawPath = ""
		if u.Path == ".." || strings.HasPrefix(u.Path, "../") || u.Path == "__redrop" || strings.HasPrefix(u.Path, "__redrop/") {
			return "", false
		}
		// Make a colon in the first segment unambiguously a relative path.
		u.Path = "./" + u.Path
	}
	return u.String(), true
}
