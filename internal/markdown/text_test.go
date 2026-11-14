package markdown

import (
	"reflect"
	"strings"
	"testing"

	"golang.org/x/net/html"
)

func TestTagNameTextSurvivesExactly(t *testing.T) {
	root := parseDoc(t, renderDoc(t, "- a\n\n| value |\n|---|\n| a |\n\n**a**\n\n[a](./x.md)\n\ninput\n\nspan\n"))
	got := map[string][]string{}
	walk(root, func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "main" {
			walk(n, func(child *html.Node) {
				if child.Type == html.ElementNode {
					switch child.Data {
					case "li", "td", "strong", "a", "p":
						got[child.Data] = append(got[child.Data], nodeText(child))
					}
				}
				if child.Type == html.TextNode && len(child.Attr) != 0 {
					t.Errorf("text node %q acquired element attributes", child.Data)
				}
			})
		}
	})
	want := map[string][]string{
		"li": {"a"}, "td": {"a"}, "strong": {"a"}, "a": {"a"},
		"p": {"a", "a", "input", "span"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("render changed literal text: got %#v, want %#v", got, want)
	}
}

func TestCommentsInsideRawBlocksDisappear(t *testing.T) {
	doc := parseDoc(t, renderDoc(t, "<div title=\"<!-- literal attribute -->\">\n<!-- hidden in block -->\nVisible\n</div>\n"))
	var text string
	walk(doc, func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "main" {
			text = nodeText(n)
		}
	})
	if strings.Contains(text, "hidden in block") || !strings.Contains(text, `<div title="<!-- literal attribute -->">`) || !strings.Contains(text, "Visible") {
		t.Fatalf("comments inside raw blocks must disappear without changing literal attributes: %q", text)
	}
}

func TestCommentsDisappearAndRawBlocksAreProse(t *testing.T) {
	doc := renderDoc(t, "<!-- markdownlint-disable -->\n\nText <!-- inline --> tail.\n\n<div>Literal</div>\n\n<script>alert(1)</script>\n")
	for _, bad := range []string{"markdownlint-disable", "<!-- inline -->", "<pre>", "<code>", "<div>Literal</div>", "<script>alert(1)</script>"} {
		if strings.Contains(doc, bad) {
			t.Errorf("unexpected rendered block content %q", bad)
		}
	}
	for _, want := range []string{"Text  tail.", "<p>&lt;div&gt;Literal&lt;/div&gt;", "<p>&lt;script&gt;alert(1)&lt;/script&gt;"} {
		if !strings.Contains(doc, want) {
			t.Errorf("missing escaped prose %q", want)
		}
	}
}
