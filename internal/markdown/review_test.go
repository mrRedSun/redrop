package markdown

import (
	"reflect"
	"strings"
	"testing"

	"golang.org/x/net/html"
)

func TestExternalNavigationAndLocalImages(t *testing.T) {
	doc := renderDoc(t, "[Docs](https://example.com/docs) <http://example.com/> <mailto:help@example.com>\n\n![Remote](https://example.com/pixel.png) ![Local](images/local.png)\n")
	for _, want := range []string{
		`href="https://example.com/docs" rel="noopener noreferrer nofollow" target="_blank"`,
		`href="http://example.com/" rel="noopener noreferrer nofollow" target="_blank"`,
		`href="mailto:help@example.com" rel="noopener noreferrer nofollow" target="_blank"`,
		`src="./images/local.png"`,
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("missing %s", want)
		}
	}
	if strings.Contains(doc, `src="https://`) {
		t.Error("external image retained")
	}
}

func TestRawHTMLSurvivesAsText(t *testing.T) {
	source := "Returns List<String> or Map<K, V> and a <b>bold</b> word.\n\nUse <kbd>Ctrl</kbd>+<kbd>C</kbd>\n\n<script>alert(1)</script>\n\n| Type |\n|---|\n| <script> |\n"
	doc := parseDoc(t, renderDoc(t, source))
	text := nodeText(doc)
	for _, want := range []string{"List<String>", "Map<K, V>", "<b>bold</b>", "<kbd>Ctrl</kbd>+<kbd>C</kbd>", "<script>alert(1)</script>"} {
		if !strings.Contains(text, want) {
			t.Errorf("lost literal HTML-looking prose %q", want)
		}
	}
	walk(doc, func(n *html.Node) {
		if n.Type == html.ElementNode && (n.Data == "kbd" || n.Data == "b") {
			t.Errorf("raw HTML became markup: %s", n.Data)
		}
	})
}

func TestLocalReferenceResult(t *testing.T) {
	result, err := Render([]byte("![Image](images/a.png) [Same](images/a.png?version=1) [Up](../data.csv) [Local](#heading) [Remote](https://example.com)"), "docs/notes.md", "docs")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"docs/images/a.png", "data.csv"}; !reflect.DeepEqual(result.LocalReferences, want) {
		t.Errorf("local references = %v, want %v", result.LocalReferences, want)
	}
}

func TestRejectedLinksArePlainText(t *testing.T) {
	doc := renderDoc(t, "[Bad](javascript&colon;alert%281%29) [Empty]() [No host](https:example.com) [Mail](mailto:)")
	if strings.Contains(doc, `href=""`) || strings.Contains(doc, `<a>Bad</a>`) || !strings.Contains(doc, `<span>Bad</span>`) || !strings.Contains(doc, `<span>Empty</span>`) {
		t.Error("rejected links should be plain text without empty or dead anchors")
	}
}

func TestPortableDocument(t *testing.T) {
	doc := renderDoc(t, "# Portable\n\n[Asset](__redrop/info.txt)\n")
	for _, bad := range []string{`rel="icon"`, `name="robots"`, `name="referrer"`, "?download", "data:image/"} {
		if strings.Contains(doc, bad) {
			t.Errorf("document contains hosting-specific content %q", bad)
		}
	}
	for _, want := range []string{`href="./notes.md" download`, `href="./__redrop/info.txt"`, `Content-Security-Policy`} {
		if !strings.Contains(doc, want) {
			t.Errorf("missing portable content %s", want)
		}
	}
}
