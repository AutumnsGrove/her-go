package layers

// Agent layer: Attached image + OCR text.
// When the user sends a photo, this tells the agent about it and
// includes any pre-flight OCR text. The agent decides whether to
// use scan_receipt (for receipts) or view_image (for the VLM).

import "strings"

func init() {
	Register(PromptLayer{
		Name:    "Attached Image",
		Order:   350,
		Stream:  StreamAgent,
		Builder: buildAgentImage,
	})
}

func buildAgentImage(ctx *LayerContext) LayerResult {
	if !ctx.HasImage {
		return LayerResult{}
	}

	var b strings.Builder
	b.WriteString("## Attached Image\n\n")
	if ctx.OCRText != "" {
		b.WriteString("The user sent a photo. Pre-flight OCR extracted the following text:\n\n")
		b.WriteString("```\n")
		b.WriteString(ctx.OCRText)
		b.WriteString("\n```\n\n")
		b.WriteString("If the OCR text is garbled or not useful, call `view_image` to see the photo with the VLM instead.\n")
	} else {
		b.WriteString("The user sent a photo. No OCR text was extracted (image may not contain text). ")
		b.WriteString("Call `view_image` to see what's in it before replying.\n")
	}

	detail := "no OCR"
	if ctx.OCRText != "" {
		detail = "with OCR"
	}
	return LayerResult{
		Content: b.String(),
		Detail:  detail,
	}
}
