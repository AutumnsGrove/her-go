package agent

import (
	"encoding/json"
	"fmt"

	"her/vision"
)

// execViewImage calls the vision LLM to describe an image the user sent.
// It uses the vision package to build a multi-modal request and send it
// to the VLM (Gemini 3 Flash via OpenRouter).
//
// The description gets accumulated in searchContext — same pattern as
// web_search results — so the reply tool can reference it when
// generating the conversational response.
func execViewImage(argsJSON string, tctx *toolContext) string {
	var args struct {
		Prompt string `json:"prompt"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("error parsing arguments: %v", err)
	}

	// Guard: no image attached to this message.
	if tctx.imageBase64 == "" {
		return "No image attached to this message. The user didn't send a photo."
	}

	// Guard: vision model not configured.
	if tctx.visionLLM == nil {
		return "Vision is not configured. Add a vision section to config.yaml to enable image understanding."
	}

	// Show a status update while the VLM works.
	if tctx.statusCallback != nil {
		_ = tctx.statusCallback("looking at the image...")
	}

	log.Info("  view_image", "prompt", args.Prompt)

	// Call the VLM through the vision package.
	result, err := vision.Describe(tctx.visionLLM, tctx.imageBase64, tctx.imageMIME, args.Prompt)
	if err != nil {
		log.Error("vision describe failed", "err", err)
		return fmt.Sprintf("Failed to analyze the image: %v", err)
	}

	// Log metrics — same pattern as other LLM calls.
	if tctx.store != nil && tctx.triggerMsgID > 0 {
		if err := tctx.store.SaveMetric(
			result.Model,
			result.PromptTokens,
			result.CompletionTokens,
			result.TotalTokens,
			result.CostUSD,
			0, // latency — not tracked at this level
			tctx.triggerMsgID,
		); err != nil {
			log.Error("saving vision metric", "err", err)
		}
	}

	log.Info("  vision result",
		"model", result.Model,
		"tokens", result.TotalTokens,
		"cost", fmt.Sprintf("$%.6f", result.CostUSD),
	)

	// Accumulate the image description in searchContext so the reply
	// tool can reference it — same pattern as web_search results.
	imageContext := fmt.Sprintf("## Image Description\n\n%s", result.Description)
	if tctx.searchContext != "" {
		tctx.searchContext += "\n\n" + imageContext
	} else {
		tctx.searchContext = imageContext
	}

	return result.Description
}
