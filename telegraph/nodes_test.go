package telegraph

import (
	"encoding/json"
	"testing"
)

func TestMarkdownToNodes_Heading(t *testing.T) {
	nodes := MarkdownToNodes("# Big Title\n\n## Subtitle\n\n### H3\n\n#### H4")

	if len(nodes) != 4 {
		t.Fatalf("expected 4 nodes, got %d", len(nodes))
	}

	// H1 and H2 both map to h3 in Telegraph.
	assertTag(t, nodes[0], "h3")
	assertTag(t, nodes[1], "h3")
	assertTag(t, nodes[2], "h3")
	assertTag(t, nodes[3], "h4")
}

func TestMarkdownToNodes_Paragraph(t *testing.T) {
	nodes := MarkdownToNodes("Hello world.\n\nSecond paragraph.")

	if len(nodes) != 2 {
		t.Fatalf("expected 2 nodes, got %d", len(nodes))
	}
	assertTag(t, nodes[0], "p")
	assertTag(t, nodes[1], "p")
}

func TestMarkdownToNodes_BoldItalic(t *testing.T) {
	nodes := MarkdownToNodes("**bold** and *italic*")

	if len(nodes) != 1 {
		t.Fatalf("expected 1 node, got %d", len(nodes))
	}

	p := nodes[0].(*Node)
	if p.Tag != "p" {
		t.Fatalf("expected p tag, got %s", p.Tag)
	}

	// Should contain: bold node, text " and ", italic node
	found := false
	for _, child := range p.Children {
		if n, ok := child.(*Node); ok && n.Tag == "b" {
			found = true
		}
	}
	if !found {
		t.Error("expected bold node in paragraph children")
	}
}

func TestMarkdownToNodes_CodeBlock(t *testing.T) {
	md := "```go\nfmt.Println(\"hello\")\n```"
	nodes := MarkdownToNodes(md)

	if len(nodes) != 1 {
		t.Fatalf("expected 1 node, got %d", len(nodes))
	}
	assertTag(t, nodes[0], "pre")
}

func TestMarkdownToNodes_List(t *testing.T) {
	md := "- item 1\n- item 2\n- item 3"
	nodes := MarkdownToNodes(md)

	if len(nodes) != 1 {
		t.Fatalf("expected 1 node, got %d", len(nodes))
	}
	assertTag(t, nodes[0], "ul")

	ul := nodes[0].(*Node)
	if len(ul.Children) != 3 {
		t.Fatalf("expected 3 list items, got %d", len(ul.Children))
	}
}

func TestMarkdownToNodes_OrderedList(t *testing.T) {
	md := "1. first\n2. second"
	nodes := MarkdownToNodes(md)

	if len(nodes) != 1 {
		t.Fatalf("expected 1 node, got %d", len(nodes))
	}
	assertTag(t, nodes[0], "ol")
}

func TestMarkdownToNodes_Link(t *testing.T) {
	md := "[click here](https://example.com)"
	nodes := MarkdownToNodes(md)

	if len(nodes) != 1 {
		t.Fatalf("expected 1 node, got %d", len(nodes))
	}

	// The link should be inside a paragraph.
	p := nodes[0].(*Node)
	found := false
	for _, child := range p.Children {
		if n, ok := child.(*Node); ok && n.Tag == "a" {
			if n.Attrs["href"] != "https://example.com" {
				t.Errorf("expected href https://example.com, got %s", n.Attrs["href"])
			}
			found = true
		}
	}
	if !found {
		t.Error("expected link node")
	}
}

func TestMarkdownToNodes_Blockquote(t *testing.T) {
	md := "> wise words"
	nodes := MarkdownToNodes(md)

	if len(nodes) != 1 {
		t.Fatalf("expected 1 node, got %d", len(nodes))
	}
	assertTag(t, nodes[0], "blockquote")
}

func TestMarkdownToNodes_EmptyInput(t *testing.T) {
	nodes := MarkdownToNodes("")
	if len(nodes) == 0 {
		t.Fatal("expected at least 1 fallback node for empty input")
	}
}

func TestMarkdownToNodes_SerializesToJSON(t *testing.T) {
	md := "# Report\n\n- item **bold**\n- item *italic*\n\n> quote\n\n```\ncode\n```"
	nodes := MarkdownToNodes(md)

	data, err := json.Marshal(nodes)
	if err != nil {
		t.Fatalf("failed to marshal to JSON: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("empty JSON output")
	}
	// Verify it's valid JSON array.
	var parsed []interface{}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
}

func TestMarkdownToNodes_NestedList(t *testing.T) {
	md := "- item 1\n  - nested a\n  - nested b\n- item 2"
	nodes := MarkdownToNodes(md)

	if len(nodes) == 0 {
		t.Fatal("expected at least 1 node")
	}
	assertTag(t, nodes[0], "ul")
}

func TestMarkdownToNodes_HorizontalRule(t *testing.T) {
	md := "Before\n\n---\n\nAfter"
	nodes := MarkdownToNodes(md)

	found := false
	for _, n := range nodes {
		if node, ok := n.(*Node); ok && node.Tag == "hr" {
			found = true
		}
	}
	if !found {
		t.Error("expected hr node")
	}
}

func TestMarkdownToNodes_InlineCode(t *testing.T) {
	md := "Use `fmt.Println` to print."
	nodes := MarkdownToNodes(md)

	if len(nodes) != 1 {
		t.Fatalf("expected 1 node, got %d", len(nodes))
	}

	p := nodes[0].(*Node)
	found := false
	for _, child := range p.Children {
		if n, ok := child.(*Node); ok && n.Tag == "code" {
			found = true
		}
	}
	if !found {
		t.Error("expected inline code node")
	}
}

func assertTag(t *testing.T, node interface{}, expected string) {
	t.Helper()
	n, ok := node.(*Node)
	if !ok {
		t.Fatalf("expected *Node, got %T", node)
	}
	if n.Tag != expected {
		t.Errorf("expected tag %q, got %q", expected, n.Tag)
	}
}
