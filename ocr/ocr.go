// Package ocr provides local text extraction from images using Apple Vision
// (via the macos-vision-ocr CLI) with a GLM-OCR fallback via LM Studio.
//
// This is the "pre-flight" layer for photos: it runs on EVERY incoming image
// before the agent decides what to do. Since Apple Vision runs on the Neural
// Engine (sub-200ms, zero network calls), it's essentially free. The agent
// reads the OCR text and routes accordingly — receipt text triggers expense
// tracking, garbled/empty text falls through to the VLM for visual description.
//
// Think of this like Python's subprocess.run() — we shell out to a CLI binary,
// capture its JSON output, and parse the result. No CGo, no system libraries
// to link against.
package ocr

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"

	"her/config"
)

// Result holds the output of an OCR extraction.
type Result struct {
	Text       string  // all extracted text, concatenated
	Confidence float64 // average confidence across all observations (0.0-1.0)
	Engine     string  // which engine produced this result: "apple-vision" or "glm-ocr"
}

// visionOCROutput matches the JSON output format of the macos-vision-ocr CLI.
// The binary outputs: {"texts": "...", "info": {...}, "observations": [...]}
// We only care about `texts` (concatenated text) and `observations` (for
// per-line confidence scores).
type visionOCROutput struct {
	Texts        string              `json:"texts"`
	Observations []visionObservation `json:"observations"`
}

// visionObservation is a single text region detected by Apple Vision.
// Each has the recognized text, a confidence score (0.0-1.0), and bounding
// box coordinates (which we don't need for receipt scanning).
type visionObservation struct {
	Text       string  `json:"text"`
	Confidence float64 `json:"confidence"`
}

// Extract runs OCR on the given image bytes and returns the extracted text.
// It writes the image to a temp file (macos-vision-ocr needs a file path),
// shells out to the CLI binary, parses the JSON output, and cleans up.
//
// If the primary engine (Apple Vision) returns low confidence or empty
// results, it falls back to GLM-OCR via LM Studio — but only if the
// fallback is configured.
//
// Returns nil result (not an error) if OCR is not configured (no binary path).
func Extract(imageBytes []byte, cfg *config.OCRConfig) (*Result, error) {
	if cfg.VisionOCRPath == "" {
		return nil, nil // OCR not configured — caller should skip gracefully
	}

	// Step 1: Write image bytes to a temp file.
	// macos-vision-ocr takes a file path as input, not stdin.
	// os.CreateTemp gives us a unique filename in the OS temp directory —
	// like Python's tempfile.NamedTemporaryFile(delete=False).
	tmpFile, err := os.CreateTemp("", "her-ocr-*.png")
	if err != nil {
		return nil, fmt.Errorf("creating temp file for OCR: %w", err)
	}
	tmpPath := tmpFile.Name()
	// defer ensures this runs when Extract() returns, even on error.
	// Same idea as Python's try/finally or a context manager.
	defer os.Remove(tmpPath)

	if _, err := tmpFile.Write(imageBytes); err != nil {
		tmpFile.Close()
		return nil, fmt.Errorf("writing image to temp file: %w", err)
	}
	tmpFile.Close() // close before shelling out — the CLI needs to read it

	// Step 2: Run Apple Vision OCR.
	result, err := runAppleVision(tmpPath, cfg.VisionOCRPath)
	if err != nil {
		// Binary not found or crashed — try fallback if configured.
		if cfg.Fallback.BaseURL != "" {
			return runGLMOCR(tmpPath, &cfg.Fallback)
		}
		return nil, fmt.Errorf("apple vision OCR failed: %w", err)
	}

	// Step 3: Check confidence. If too low, try GLM-OCR fallback.
	threshold := cfg.ConfidenceThreshold
	if threshold == 0 {
		threshold = 0.5 // sensible default
	}
	if result.Text == "" || result.Confidence < threshold {
		if cfg.Fallback.BaseURL != "" {
			fallbackResult, fbErr := runGLMOCR(tmpPath, &cfg.Fallback)
			if fbErr == nil && fallbackResult.Text != "" {
				return fallbackResult, nil
			}
			// Fallback also failed — return whatever Apple Vision got.
		}
	}

	return result, nil
}

// IsAvailable checks if the OCR binary exists and is executable.
// Called at bot startup to set the ocrEnabled flag. Quick check — just
// looks up the binary on PATH, doesn't actually run it.
func IsAvailable(cfg *config.OCRConfig) bool {
	if cfg.VisionOCRPath == "" {
		return false
	}
	_, err := exec.LookPath(cfg.VisionOCRPath)
	return err == nil
}

// runAppleVision calls the macos-vision-ocr CLI binary on the image file.
// The binary outputs JSON to stdout: {texts, info, observations[]}.
//
// exec.Command is Go's subprocess.run() equivalent. We capture stdout
// into a buffer and parse the JSON. If the binary isn't found or returns
// a non-zero exit code, we get an error.
func runAppleVision(imagePath, binaryPath string) (*Result, error) {
	// #nosec G204 — binaryPath comes from config, not user input.
	// The CLI expects --img <path>, not a positional argument.
	cmd := exec.Command(binaryPath, "--img", imagePath)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("macos-vision-ocr: %w (stderr: %s)", err, stderr.String())
	}

	var output visionOCROutput
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		return nil, fmt.Errorf("parsing macos-vision-ocr output: %w", err)
	}

	// Calculate average confidence across all observations.
	var avgConfidence float64
	if len(output.Observations) > 0 {
		var total float64
		for _, obs := range output.Observations {
			total += obs.Confidence
		}
		avgConfidence = total / float64(len(output.Observations))
	}

	return &Result{
		Text:       output.Texts,
		Confidence: avgConfidence,
		Engine:     "apple-vision",
	}, nil
}

// runGLMOCR calls the GLM-OCR model via LM Studio's OpenAI-compatible API.
// GLM-OCR is a purpose-built OCR model (0.9B params, MIT licensed) that
// excels at document text extraction. It's used as a fallback when Apple
// Vision struggles — e.g., unusual fonts, heavy glare, non-Latin scripts.
//
// TODO: Implement GLM-OCR fallback via LM Studio API.
// For now, returns an error indicating the fallback isn't implemented yet.
// The Apple Vision primary engine handles the vast majority of cases.
func runGLMOCR(imagePath string, cfg *config.OCRFallback) (*Result, error) {
	// GLM-OCR fallback is not yet implemented. Apple Vision handles most
	// cases well. We'll add this when we encounter receipts it can't read.
	return nil, fmt.Errorf("glm-ocr fallback not yet implemented")
}
