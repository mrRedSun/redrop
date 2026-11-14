package markdown

import (
	"bytes"
	"io"

	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/renderer"
	"github.com/yuin/goldmark/util"
	"golang.org/x/net/html"
)

// rawTextRenderer preserves HTML-looking prose without enabling raw HTML.
// Comments are omitted; other tags stay literal text in inline prose or paragraphs.
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
	if n.Segments.Len() > 0 {
		first := n.Segments.At(0)
		if bytes.HasPrefix(first.Value(source), []byte("<!--")) {
			return ast.WalkContinue, nil
		}
	}
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
	if n.HTMLBlockType == ast.HTMLBlockType2 {
		return ast.WalkContinue, nil
	}
	if _, err := w.WriteString("<p>"); err != nil {
		return ast.WalkStop, err
	}
	var content bytes.Buffer
	for i := 0; i < n.Lines().Len(); i++ {
		line := n.Lines().At(i)
		content.Write(line.Value(source))
	}
	if n.HasClosure() {
		content.Write(n.ClosureLine.Value(source))
	}
	// Tokenize only to omit actual comments inside a larger HTML block. Raw
	// token bytes preserve tags and attributes exactly before escaping them.
	tokens := html.NewTokenizer(&content)
	for {
		kind := tokens.Next()
		if kind == html.ErrorToken {
			if err := tokens.Err(); err != io.EOF {
				return ast.WalkStop, err
			}
			break
		}
		if kind != html.CommentToken || !bytes.HasPrefix(tokens.Raw(), []byte("<!--")) {
			if _, err := w.Write(util.EscapeHTML(tokens.Raw())); err != nil {
				return ast.WalkStop, err
			}
		}
	}
	_, err := w.WriteString("</p>\n")
	return ast.WalkContinue, err
}
