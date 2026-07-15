// Package vision provides image understanding via a vision-language model (VLM).
// It builds multi-modal messages (text + image) and sends them to the VLM
// through the standard LLM client. The VLM describes what it sees, and
// the description flows back into the agent's context for reply generation.
//
// This is similar to how search/tavily.go wraps the Tavily API — a focused
// package that knows how to talk to one specific service. The LLM client
// does the actual HTTP work; this package just knows how to format the
// multi-modal request.
package vision

import (
	"fmt"

	"her/llm"
)

// DescribeResult holds the VLM's image description plus token usage
// for metrics logging. Same shape as other tool results in the project.
type DescribeResult struct {
	Description      string
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	CostUSD          float64
	Model            string
	UsedFallback     bool // true if primary vision model failed and fallback was used
}

// Describe sends an image to the VLM and returns a natural language
// description. The prompt guides what the model focuses on — e.g.,
// "describe this photo", "what food is this", "read any text".
//
// imageBase64 is the raw base64-encoded image data (no data: prefix).
// imageMIME is the MIME type (e.g., "image/jpeg", "image/png").
// The function builds the full data URI internally.
func Describe(client *llm.Client, imageBase64, imageMIME, prompt string) (*DescribeResult, error) {
	if imageBase64 == "" {
		return nil, fmt.Errorf("no image data provided")
	}

	// Build a data: URI — the OpenAI vision API accepts inline base64
	// images this way. No need to host the image anywhere; it travels
	// inside the JSON request body.
	dataURI := "data:" + imageMIME + ";base64," + imageBase64
	return describe(client, dataURI, prompt)
}

// DescribeURL sends a remote image URL to the VLM and returns a natural
// language description, without downloading or re-encoding the image
// locally. This is what lets the agent look at images it discovers on the
// web — a book cover from search_books' CoverURL, a photo from a
// web_search result, a link the user pastes into chat — not just photos
// the user uploaded directly to Telegram.
//
// The OpenAI-compatible vision API fetches the URL server-side, so we just
// pass it straight through as the "image_url" content part instead of
// building a data: URI.
func DescribeURL(client *llm.Client, imageURL, prompt string) (*DescribeResult, error) {
	if imageURL == "" {
		return nil, fmt.Errorf("no image URL provided")
	}
	return describe(client, imageURL, prompt)
}

// describe is the shared implementation behind Describe and DescribeURL.
// imageURL is either a data: URI (inline base64) or a regular https://
// link — the vision API treats both the same way as an "image_url" part.
func describe(client *llm.Client, imageURL, prompt string) (*DescribeResult, error) {
	if client == nil {
		return nil, fmt.Errorf("vision client is not configured")
	}

	// Default prompt if none given.
	if prompt == "" {
		prompt = "Describe this image in detail."
	}

	messages := []llm.ChatMessage{
		{
			Role: "user",
			ContentParts: []llm.ContentPart{
				{Type: "text", Text: prompt},
				{Type: "image_url", ImageURL: &llm.ImageURL{URL: imageURL}},
			},
		},
	}

	resp, err := client.ChatCompletion(messages)
	if err != nil {
		return nil, fmt.Errorf("vision API call failed: %w", err)
	}

	return &DescribeResult{
		Description:      resp.Content,
		PromptTokens:     resp.PromptTokens,
		CompletionTokens: resp.CompletionTokens,
		TotalTokens:      resp.TotalTokens,
		CostUSD:          resp.CostUSD,
		Model:            resp.Model,
		UsedFallback:     resp.UsedFallback,
	}, nil
}
