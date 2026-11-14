package markdown

import (
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/util"
)

func rejectedImageSources(document ast.Node, base string) []string {
	var rejected []string
	seen := map[string]bool{}
	_ = ast.Walk(document, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		image, ok := node.(*ast.Image)
		if entering && ok {
			// Use the same URL normalization as Goldmark's image renderer, but
			// retain the original source before dangerous schemes become src="".
			destination := string(util.URLEscape(image.Destination, true))
			original := string(image.Destination)
			if _, allowed := localURL(destination, base); !allowed && !seen[original] {
				rejected = append(rejected, original)
				seen[original] = true
			}
		}
		return ast.WalkContinue, nil
	})
	return rejected
}
