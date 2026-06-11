package telegraph

import (
	"bytes"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"
)

// Node is a Telegraph DOM element. Telegraph pages are represented as a
// tree of these nodes — same idea as HTML DOM but with a restricted tag set.
//
// A node is either:
//   - A text node: just a string (represented as a raw string in the JSON array)
//   - An element node: tag + optional attrs + children
//
// The Telegraph API accepts content as a JSON array where each element is
// either a string (text node) or a Node object (element node).
type Node struct {
	Tag      string            `json:"tag"`
	Attrs    map[string]string `json:"attrs,omitempty"`
	Children []interface{}     `json:"children,omitempty"` // string or *Node
}

// MarkdownToNodes converts a markdown string into a slice of Telegraph
// DOM nodes. Uses goldmark to parse the AST and walks it to emit
// Telegraph-compatible elements.
//
// Supported mappings:
//
//	# H1, ## H2 → h3 (Telegraph only has h3/h4)
//	### H3      → h3
//	#### H4+    → h4
//	paragraph   → p
//	**bold**    → b
//	*italic*    → em
//	`code`      → code
//	```block``` → pre
//	> quote     → blockquote
//	- list      → ul > li
//	1. list     → ol > li
//	[text](url) → a
//	---         → hr
func MarkdownToNodes(md string) []interface{} {
	source := []byte(md)
	parser := goldmark.DefaultParser()
	doc := parser.Parse(text.NewReader(source))

	var nodes []interface{}
	for child := doc.FirstChild(); child != nil; child = child.NextSibling() {
		if n := convertBlock(child, source); n != nil {
			nodes = append(nodes, n)
		}
	}

	if len(nodes) == 0 {
		nodes = append(nodes, &Node{Tag: "p", Children: []interface{}{md}})
	}
	return nodes
}

func convertBlock(node ast.Node, source []byte) interface{} {
	switch n := node.(type) {
	case *ast.Heading:
		tag := "h3"
		if n.Level >= 4 {
			tag = "h4"
		}
		return &Node{Tag: tag, Children: convertInlineChildren(n, source)}

	case *ast.Paragraph:
		children := convertInlineChildren(n, source)
		if len(children) == 0 {
			return nil
		}
		return &Node{Tag: "p", Children: children}

	case *ast.FencedCodeBlock:
		var buf bytes.Buffer
		lines := n.Lines()
		for i := 0; i < lines.Len(); i++ {
			line := lines.At(i)
			buf.Write(line.Value(source))
		}
		code := &Node{Tag: "code", Children: []interface{}{buf.String()}}
		return &Node{Tag: "pre", Children: []interface{}{code}}

	case *ast.CodeBlock:
		var buf bytes.Buffer
		lines := n.Lines()
		for i := 0; i < lines.Len(); i++ {
			line := lines.At(i)
			buf.Write(line.Value(source))
		}
		code := &Node{Tag: "code", Children: []interface{}{buf.String()}}
		return &Node{Tag: "pre", Children: []interface{}{code}}

	case *ast.Blockquote:
		var children []interface{}
		for child := n.FirstChild(); child != nil; child = child.NextSibling() {
			if c := convertBlock(child, source); c != nil {
				children = append(children, c)
			}
		}
		return &Node{Tag: "blockquote", Children: children}

	case *ast.List:
		tag := "ul"
		if n.IsOrdered() {
			tag = "ol"
		}
		var items []interface{}
		for child := n.FirstChild(); child != nil; child = child.NextSibling() {
			if li, ok := child.(*ast.ListItem); ok {
				var liChildren []interface{}
				for liChild := li.FirstChild(); liChild != nil; liChild = liChild.NextSibling() {
					switch p := liChild.(type) {
					case *ast.Paragraph:
						// Loose list items wrap content in Paragraph.
						liChildren = append(liChildren, convertInlineChildren(p, source)...)
					case *ast.TextBlock:
						// Tight list items (no blank lines between) use
						// TextBlock instead of Paragraph — same inline
						// extraction logic applies.
						liChildren = append(liChildren, convertInlineChildren(p, source)...)
					default:
						if c := convertBlock(liChild, source); c != nil {
							liChildren = append(liChildren, c)
						}
					}
				}
				items = append(items, &Node{Tag: "li", Children: liChildren})
			}
		}
		return &Node{Tag: tag, Children: items}

	case *ast.ThematicBreak:
		return &Node{Tag: "hr"}

	case *ast.HTMLBlock:
		// Pass raw HTML as text — Telegraph won't render it but at
		// least the content isn't lost.
		var buf bytes.Buffer
		lines := n.Lines()
		for i := 0; i < lines.Len(); i++ {
			line := lines.At(i)
			buf.Write(line.Value(source))
		}
		trimmed := strings.TrimSpace(buf.String())
		if trimmed != "" {
			return &Node{Tag: "p", Children: []interface{}{trimmed}}
		}
		return nil

	default:
		// Unknown block type — try to extract text content.
		var children []interface{}
		for child := n.FirstChild(); child != nil; child = child.NextSibling() {
			if c := convertBlock(child, source); c != nil {
				children = append(children, c)
			}
		}
		if len(children) > 0 {
			return &Node{Tag: "p", Children: children}
		}
		return nil
	}
}

func convertInlineChildren(node ast.Node, source []byte) []interface{} {
	var children []interface{}
	for child := node.FirstChild(); child != nil; child = child.NextSibling() {
		children = append(children, convertInline(child, source)...)
	}
	return children
}

func convertInline(node ast.Node, source []byte) []interface{} {
	switch n := node.(type) {
	case *ast.Text:
		s := string(n.Segment.Value(source))
		if n.HardLineBreak() || n.SoftLineBreak() {
			return []interface{}{s, &Node{Tag: "br"}}
		}
		return []interface{}{s}

	case *ast.Emphasis:
		tag := "em"
		if n.Level == 2 {
			tag = "b"
		}
		return []interface{}{&Node{Tag: tag, Children: convertInlineChildren(n, source)}}

	case *ast.CodeSpan:
		var buf bytes.Buffer
		for child := n.FirstChild(); child != nil; child = child.NextSibling() {
			if t, ok := child.(*ast.Text); ok {
				buf.Write(t.Segment.Value(source))
			}
		}
		return []interface{}{&Node{Tag: "code", Children: []interface{}{buf.String()}}}

	case *ast.Link:
		attrs := map[string]string{"href": string(n.Destination)}
		return []interface{}{&Node{
			Tag:      "a",
			Attrs:    attrs,
			Children: convertInlineChildren(n, source),
		}}

	case *ast.AutoLink:
		url := string(n.URL(source))
		attrs := map[string]string{"href": url}
		return []interface{}{&Node{
			Tag:      "a",
			Attrs:    attrs,
			Children: []interface{}{url},
		}}

	case *ast.Image:
		// Telegraph supports images via <figure><img>.
		attrs := map[string]string{"src": string(n.Destination)}
		return []interface{}{&Node{
			Tag: "figure",
			Children: []interface{}{&Node{
				Tag:   "img",
				Attrs: attrs,
			}},
		}}

	case *ast.RawHTML:
		// Inline HTML — just extract the text.
		var buf bytes.Buffer
		for i := 0; i < n.Segments.Len(); i++ {
			seg := n.Segments.At(i)
			buf.Write(seg.Value(source))
		}
		return []interface{}{buf.String()}

	default:
		// Unknown inline — recurse into children.
		return convertInlineChildren(node, source)
	}
}
