package markdown

import (
	"net/url"
	"strings"
	"unicode"

	"golang.org/x/net/html"
)

// navigationURL permits deliberate external navigation, never external assets.
func navigationURL(value, base string) (string, bool, bool) {
	if value == "" || strings.TrimSpace(value) != value || strings.Contains(value, "\\") || strings.IndexFunc(value, unicode.IsControl) >= 0 {
		return "", false, false
	}
	u, err := url.Parse(value)
	if err != nil {
		return "", false, false
	}
	if u.IsAbs() {
		decoded, err := url.PathUnescape(value)
		if err != nil || strings.Contains(decoded, "\\") || strings.IndexFunc(decoded, unicode.IsControl) >= 0 || u.User != nil {
			return "", false, false
		}
		u.Scheme = strings.ToLower(u.Scheme)
		switch u.Scheme {
		case "http", "https":
			if u.Hostname() != "" && u.Opaque == "" {
				return u.String(), true, true
			}
		case "mailto":
			if u.Opaque != "" && u.Host == "" {
				return u.String(), true, true
			}
		}
		return "", false, false
	}
	local, ok := localURL(value, base)
	return local, false, ok
}

func localReferences(root *html.Node) []string {
	var references []string
	seen := map[string]bool{}
	walk(root, func(n *html.Node) {
		for _, a := range n.Attr {
			if (a.Key != "href" && a.Key != "src") || !strings.HasPrefix(a.Val, "./") {
				continue
			}
			u, err := url.Parse(a.Val)
			if err != nil {
				continue
			}
			name := strings.TrimPrefix(u.Path, "./")
			if name != "" && name != "." && !seen[name] {
				references = append(references, name)
				seen[name] = true
			}
		}
	})
	return references
}
