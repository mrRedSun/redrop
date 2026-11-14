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
	"github.com/yuin/goldmark/renderer"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
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

// Result contains the portable document and the bundle-relative asset/link paths
// it references. Callers decide which references exist in their upload. Rejected
// image sources retain their original Markdown destinations for diagnostics.
type Result struct {
	HTML                 []byte
	LocalReferences      []string
	RejectedImageSources []string
}

// Render returns a self-contained HTML page. name and base are slash-separated
// paths inside the bundle. base rebases relative links when the source is nested.
func Render(source []byte, name, base string) (Result, error) {
	if len(source) > MaxSourceBytes {
		return Result{}, fmt.Errorf("Markdown exceeds %d MiB; use --render none to share it raw", MaxSourceBytes>>20)
	}
	var raw bytes.Buffer
	md := goldmark.New(
		goldmark.WithExtensions(extension.GFM),
		goldmark.WithRendererOptions(renderer.WithNodeRenderers(util.Prioritized(rawTextRenderer{}, 100))),
	) // Raw HTML is always escaped, never enabled.
	document := md.Parser().Parse(text.NewReader(source))
	rejected := rejectedImageSources(document, base)
	if err := md.Renderer().Render(&raw, source, document); err != nil {
		return Result{}, fmt.Errorf("render Markdown: %w", err)
	}
	nodes, err := html.ParseFragment(&raw, &html.Node{Type: html.ElementNode, Data: "div", DataAtom: atom.Div})
	if err != nil {
		return Result{}, fmt.Errorf("parse Markdown output: %w", err)
	}
	root := &html.Node{Type: html.ElementNode, Data: "div"}
	for _, n := range nodes {
		root.AppendChild(n)
	}
	highlight(root)
	sanitize(root, base)
	references := localReferences(root)
	headings, title := assignHeadingIDs(root, name)
	var body bytes.Buffer
	for n := root.FirstChild; n != nil; n = n.NextSibling {
		if err := html.Render(&body, n); err != nil {
			return Result{}, err
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
	}{title, name, (&url.URL{Path: "./" + name}).String(), scriptHash, template.HTML(body.String()), headings}) // body passed the allowlist above
	return Result{HTML: out.Bytes(), LocalReferences: references, RejectedImageSources: rejected}, err
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
		if n.Type == html.TextNode {
			n = next
			continue
		}
		if n.Type != html.ElementNode || !strings.Contains(allowed, " "+n.Data+" ") {
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
			case n.Data == "a" && a.Key == "href":
				if value, external, ok := navigationURL(a.Val, base); ok {
					n.Attr = append(n.Attr, html.Attribute{Key: "href", Val: value})
					if external {
						n.Attr = append(n.Attr, html.Attribute{Key: "rel", Val: "noopener noreferrer nofollow"}, html.Attribute{Key: "target", Val: "_blank"})
					}
				}
			case n.Data == "img" && a.Key == "src":
				if value, ok := localURL(a.Val, base); ok {
					n.Attr = append(n.Attr, html.Attribute{Key: "src", Val: value})
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
		if n.Data == "a" {
			hasHref := false
			for _, a := range n.Attr {
				hasHref = hasHref || a.Key == "href"
			}
			if !hasHref {
				n.Data, n.DataAtom = "span", atom.Span
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
	if value == "" || strings.ContainsAny(value, "\\") || strings.IndexFunc(value, unicode.IsControl) >= 0 {
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
		if u.Path == ".." || strings.HasPrefix(u.Path, "../") {
			return "", false
		}
		// Make a colon in the first segment unambiguously a relative path.
		u.Path = "./" + u.Path
	}
	if u.Path == "" && u.RawQuery != "" {
		u.Path = "./"
	}
	result := u.String()
	return result, result != ""
}
