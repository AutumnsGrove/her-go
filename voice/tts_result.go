package voice

// SynthesizeResult holds the synthesized audio bytes and associated cost.
// Mirrors TranscribeResult from stt.go — local engines return zero cost,
// remote engines (OpenRouter, ElevenLabs) return actual spend.
type SynthesizeResult struct {
	Audio []byte  // OGG/Opus audio bytes ready for Telegram
	Cost  float64 // USD cost from the TTS provider (0 for local engines)
}
