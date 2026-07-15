// Package view_image implements the view_image tool — analyzes an image the
// user sent using the vision language model (Gemini 3 Flash via OpenRouter).
//
// The description is accumulated in ctx.SearchContext so the reply tool can
// reference it when generating the conversational response. This follows the
// same pattern as web_search results — the description flows into the reply
// prompt automatically.
package view_image

import (
	"encoding/json"
	"fmt"
	"strings"

	"her/logger"
	"her/memory"
	"her/tools"
	"her/vision"
)

var log = logger.WithPrefix("tools/view_image")

func init() {
	tools.Register("view_image", Handle)
}

// Handle calls the vision LLM to describe the image attached to the current
// message. The result is stored in ctx.SearchContext for the reply tool.
//
// Guards: returns an error string (not a Go error) if no image is attached
// or if the vision model isn't configured. The agent sees these as tool
// results and can decide how to respond.
func Handle(argsJSON string, ctx *tools.Context) string {
	var args struct {
		Prompt   string `json:"prompt"`
		ImageURL string `json:"image_url"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("error parsing arguments: %v", err)
	}

	// Guard: image_url must be a real link, not a data: URI or garbage —
	// those only make sense coming from ctx.ImageBase64, which already
	// has its own path below.
	if args.ImageURL != "" && !strings.HasPrefix(args.ImageURL, "http://") && !strings.HasPrefix(args.ImageURL, "https://") {
		return "image_url must be a direct http(s):// link to an image."
	}

	// Guard: no image_url given and no photo attached to this message.
	if args.ImageURL == "" && ctx.ImageBase64 == "" {
		return "No image attached to this message and no image_url given. Either the user needs to send a photo, or you need to pass a URL (e.g. from search_books or web_search)."
	}

	// Guard: vision model not configured.
	if ctx.VisionLLM == nil {
		return "Vision is not configured. Add a vision section to config.yaml to enable image understanding."
	}

	// Show a status update while the VLM works.
	if ctx.StatusCallback != nil {
		_ = ctx.StatusCallback("looking at the image...")
	}

	log.Info("  view_image", "prompt", args.Prompt, "image_url", args.ImageURL)

	// Call the VLM through the vision package. This builds a multimodal
	// request (image + text prompt) and sends it to the vision model.
	// Prefer image_url when given — it lets the agent look at images it
	// discovered on the web without ever downloading them itself.
	var result *vision.DescribeResult
	var err error
	if args.ImageURL != "" {
		result, err = vision.DescribeURL(ctx.VisionLLM, args.ImageURL, args.Prompt)
	} else {
		result, err = vision.Describe(ctx.VisionLLM, ctx.ImageBase64, ctx.ImageMIME, args.Prompt)
	}
	if err != nil {
		log.Error("vision describe failed", "err", err)
		return fmt.Sprintf("Failed to analyze the image: %v", err)
	}

	// Log metrics — same pattern as other LLM calls.
	if ctx.Store != nil && ctx.TriggerMsgID > 0 {
		if err := ctx.Store.SaveMetric(memory.MetricInput{
			Model:            result.Model,
			PromptTokens:     result.PromptTokens,
			CompletionTokens: result.CompletionTokens,
			TotalTokens:      result.TotalTokens,
			CostUSD:          result.CostUSD,
			MessageID:        ctx.TriggerMsgID,
			IsFallback:       result.UsedFallback,
			AgentRole:        memory.RoleVision,
			// DescribeResult has no cache/provider fields — zero values for those
		}); err != nil {
			log.Error("saving vision metric", "err", err)
		}
	}

	log.Info("  vision result",
		"model", result.Model,
		"tokens", result.TotalTokens,
		"cost", fmt.Sprintf("$%.6f", result.CostUSD),
	)

	// Persist the VLM description to the messages table so we have a
	// permanent text record of what the bot "saw" in the image.
	if ctx.Store != nil && ctx.TriggerMsgID > 0 {
		if err := ctx.Store.UpdateMessageMedia(ctx.TriggerMsgID, "", result.Description); err != nil {
			log.Error("saving media description", "err", err)
		}
	}

	// Accumulate the image description in SearchContext so the reply
	// tool can reference it — same pattern as web_search results.
	imageContext := fmt.Sprintf("## Image Description\n\n%s", result.Description)
	if ctx.SearchContext != "" {
		ctx.SearchContext += "\n\n" + imageContext
	} else {
		ctx.SearchContext = imageContext
	}

	return result.Description
}
