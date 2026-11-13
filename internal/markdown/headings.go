package markdown

import (
	"net/url"
	"path"
	"strconv"
	"strings"
	"unicode"

	"golang.org/x/net/html"
)

func assignHeadingIDs(root *html.Node, name string) ([]heading, string) {
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
	return headings, title
}
