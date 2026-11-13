package markdown

import (
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/renderer"
	"github.com/yuin/goldmark/util"
)

// rawTextRenderer preserves HTML-looking prose without enabling raw HTML.
// Inline tags stay literal text; blocks become escaped, copyable code blocks.
type rawTextRenderer struct{}

func (rawTextRenderer) RegisterFuncs(reg renderer.NodeRendererFuncRegisterer) {
	reg.Register(ast.KindRawHTML, renderRawText)
	reg.Register(ast.KindHTMLBlock, renderHTMLText)
}

func renderRawText(w util.BufWriter, source []byte, node ast.Node, entering bool) (ast.WalkStatus, error) {
	if !entering {
		return ast.WalkContinue, nil
	}
	n := node.(*ast.RawHTML)
	for i := 0; i < n.Segments.Len(); i++ {
		segment := n.Segments.At(i)
		if _, err := w.Write(util.EscapeHTML(segment.Value(source))); err != nil {
			return ast.WalkStop, err
		}
	}
	return ast.WalkContinue, nil
}

func renderHTMLText(w util.BufWriter, source []byte, node ast.Node, entering bool) (ast.WalkStatus, error) {
	if !entering {
		return ast.WalkContinue, nil
	}
	n := node.(*ast.HTMLBlock)
	if _, err := w.WriteString("<pre><code>"); err != nil {
		return ast.WalkStop, err
	}
	for i := 0; i < n.Lines().Len(); i++ {
		line := n.Lines().At(i)
		if _, err := w.Write(util.EscapeHTML(line.Value(source))); err != nil {
			return ast.WalkStop, err
		}
	}
	if n.HasClosure() {
		if _, err := w.Write(util.EscapeHTML(n.ClosureLine.Value(source))); err != nil {
			return ast.WalkStop, err
		}
	}
	_, err := w.WriteString("</code></pre>\n")
	return ast.WalkContinue, err
}
