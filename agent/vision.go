package agent

import (
	"encoding/json"
	"fmt"

	"her/tools"
	"her/vision"
)

// execViewImage calls the vision LLM to describe an image the user sent.
// It uses the vision package to build a multi-modal request and send it
// to the VLM (Gemini 3 Flash via OpenRouter).
//
// The description gets accumulated in searchContext — same pattern as
// web_search results — so the reply tool can reference it when
// generating the conversational response.
func execViewImage(argsJSON string, tctx *tools.Context) string {
	var args struct {
		Prompt string `json:"prompt"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("error parsing arguments: %v", err)
	}

	// Guard: no image attached to this message.
	if tctx.ImageBase64 == "" {
		return "No image attached to this message. The user didn't send a photo."
	}

	// Guard: vision model not configured.
	if tctx.VisionLLM == nil {
		return "Vision is not configured. Add a vision section to config.yaml to enable image understanding."
	}

	// Show a status update while the VLM works.
	if tctx.StatusCallback != nil {
		_ = tctx.StatusCallback("looking at the image...")
	}

	log.Info("  view_image", "prompt", args.Prompt)

	// Call the VLM through the vision package.
	result, err := vision.Describe(tctx.VisionLLM, tctx.ImageBase64, tctx.ImageMIME, args.Prompt)
	if err != nil {
		log.Error("vision describe failed", "err", err)
		return fmt.Sprintf("Failed to analyze the image: %v", err)
	}

	// Log metrics — same pattern as other LLM calls.
	if tctx.Store != nil && tctx.TriggerMsgID > 0 {
		if err := tctx.Store.SaveMetric(
			result.Model,
			result.PromptTokens,
			result.CompletionTokens,
			result.TotalTokens,
			result.CostUSD,
			0, // latency — not tracked at this level
			tctx.TriggerMsgID,
		); err != nil {
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
	if tctx.Store != nil && tctx.TriggerMsgID > 0 {
		if err := tctx.Store.UpdateMessageMedia(tctx.TriggerMsgID, "", result.Description); err != nil {
			log.Error("saving media description", "err", err)
		}
	}

	// Accumulate the image description in searchContext so the reply
	// tool can reference it — same pattern as web_search results.
	imageContext := fmt.Sprintf("## Image Description\n\n%s", result.Description)
	if tctx.SearchContext != "" {
		tctx.SearchContext += "\n\n" + imageContext
	} else {
		tctx.SearchContext = imageContext
	}

	return result.Description
}
