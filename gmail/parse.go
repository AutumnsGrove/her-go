package gmail

import (
	"encoding/base64"
	"regexp"
	"strings"

	gm "google.golang.org/api/gmail/v1"
)

// extractBody walks a Gmail message's MIME parts to find readable text.
// Prefers text/plain over text/html. Falls back to HTML with tags stripped.
// Returns the body text and a list of attachment filenames.
func extractBody(msg *gm.Message) (body string, attachments []string) {
	if msg.Payload == nil {
		return "", nil
	}

	var plain, html string
	walkParts(msg.Payload, &plain, &html, &attachments)

	if plain != "" {
		return strings.TrimSpace(plain), attachments
	}
	if html != "" {
		return strings.TrimSpace(stripHTML(html)), attachments
	}
	return "", attachments
}

// walkParts recursively traverses MIME parts. Emails are nested trees —
// a multipart/mixed contains multipart/alternative which contains
// text/plain and text/html. We walk the whole tree to find all the parts.
func walkParts(part *gm.MessagePart, plain, html *string, attachments *[]string) {
	mime := part.MimeType

	// Leaf node with content
	if part.Body != nil && part.Body.Data != "" {
		decoded := decodeBase64URL(part.Body.Data)
		switch {
		case mime == "text/plain" && *plain == "":
			*plain = decoded
		case mime == "text/html" && *html == "":
			*html = decoded
		}
	}

	// Track attachments by filename (don't download content)
	if part.Filename != "" {
		*attachments = append(*attachments, part.Filename)
	}

	// Recurse into child parts
	for _, child := range part.Parts {
		walkParts(child, plain, html, attachments)
	}
}

// decodeBase64URL decodes Gmail's URL-safe base64 encoding. Gmail uses
// base64url (RFC 4648 §5) without padding — Go's base64.URLEncoding
// expects padding, so RawURLEncoding is the right choice.
func decodeBase64URL(s string) string {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return ""
	}
	return string(b)
}

var (
	reHTMLTag     = regexp.MustCompile(`<[^>]*>`)
	reWhitespace  = regexp.MustCompile(`\s{3,}`)
	reHTMLEntity  = regexp.MustCompile(`&[a-zA-Z]+;|&#[0-9]+;`)
)

// stripHTML removes tags and collapses whitespace. Good enough for
// extracting readable text from email HTML — we don't need to render
// anything, just make it readable for an LLM.
func stripHTML(s string) string {
	s = strings.ReplaceAll(s, "<br>", "\n")
	s = strings.ReplaceAll(s, "<br/>", "\n")
	s = strings.ReplaceAll(s, "<br />", "\n")
	s = strings.ReplaceAll(s, "</p>", "\n\n")
	s = strings.ReplaceAll(s, "</div>", "\n")
	s = reHTMLTag.ReplaceAllString(s, "")
	s = reHTMLEntity.ReplaceAllStringFunc(s, decodeEntity)
	s = reWhitespace.ReplaceAllString(s, "\n\n")
	return s
}

// decodeEntity handles common HTML entities. Not exhaustive — just the
// ones that show up frequently in emails.
func decodeEntity(entity string) string {
	switch entity {
	case "&amp;":
		return "&"
	case "&lt;":
		return "<"
	case "&gt;":
		return ">"
	case "&quot;":
		return "\""
	case "&apos;":
		return "'"
	case "&nbsp;":
		return " "
	default:
		return entity
	}
}
