#!/usr/bin/env python3
"""Minimal TTS server using Piper.

Exposes a single POST /v1/audio/speech endpoint (OpenAI-compatible).
Accepts JSON with text + voice, returns WAV audio bytes.

Usage:
    uv run scripts/tts_server.py                          # defaults
    uv run scripts/tts_server.py --port 8766

Voice models live in scripts/piper-voices/. Run `her setup` to download them,
or grab them manually from https://huggingface.co/rhasspy/piper-voices
"""
# /// script
# requires-python = ">=3.11,<3.14"
# dependencies = [
#     "piper-tts>=1.4",
#     "numpy",
#     "fastapi>=0.100",
#     "uvicorn>=0.22",
# ]
# ///

import argparse
import io
import logging
import re
import time
import wave
from pathlib import Path

import numpy as np
import uvicorn
from fastapi import FastAPI
from fastapi.responses import Response
from pydantic import BaseModel

logging.basicConfig(level=logging.INFO)
log = logging.getLogger("tts-server")

app = FastAPI(title="Piper TTS Server")

# Global voice reference — loaded once at startup.
_voice = None
_voice_name = None

# Duration of silence inserted between paragraphs (double newlines).
PARAGRAPH_PAUSE_MS = 500
# Duration of silence inserted at single newlines.
LINE_PAUSE_MS = 250


class SpeechRequest(BaseModel):
    model: str = ""
    input: str
    voice: str = "en_GB-southern_english_female-low"
    speed: float = 1.0
    response_format: str = "wav"


def make_silence(duration_ms: int, sample_rate: int) -> bytes:
    """Create silent PCM16 bytes of the given duration."""
    n_samples = int(sample_rate * duration_ms / 1000)
    return b"\x00\x00" * n_samples


def clean_for_tts(text: str) -> str:
    """Clean text for better TTS pronunciation.

    Handles characters and words that Piper struggles with.
    """
    # Em-dashes (—) and en-dashes (–) → comma + pause. The model isn't
    # supposed to use these, but they slip through sometimes.
    text = re.sub(r"\s*[—–]\s*", ", ", text)

    # "Huh" sounds robotic in Piper — replace with a more natural filler.
    text = re.sub(r"\bhuh\b", "hmm", text, flags=re.IGNORECASE)

    # Ellipsis → period (Piper handles sentence-end pauses better than ...)
    text = re.sub(r"\.{2,}", ".", text)

    return text


def preprocess_text(text: str) -> list[dict]:
    """Split text on newlines into chunks with pause hints.

    Returns a list of dicts:
      {"text": "...", "pause_after_ms": 500}

    Double newlines (paragraph breaks) get a longer pause.
    Single newlines get a shorter pause.
    The last chunk has no pause after it.
    """
    text = text.strip()
    text = re.sub(r"\n{3,}", "\n\n", text)

    paragraphs = text.split("\n\n")
    chunks = []

    for i, para in enumerate(paragraphs):
        lines = para.split("\n")
        for j, line in enumerate(lines):
            line = line.strip()
            if not line:
                continue
            is_last_line_in_para = j == len(lines) - 1
            is_last_para = i == len(paragraphs) - 1

            if is_last_para and is_last_line_in_para:
                pause = 0
            elif is_last_line_in_para:
                pause = PARAGRAPH_PAUSE_MS
            else:
                pause = LINE_PAUSE_MS

            chunks.append({"text": line, "pause_after_ms": pause})

    return chunks


@app.post("/v1/audio/speech")
async def speech(req: SpeechRequest):
    if _voice is None:
        return Response(content="Model not loaded", status_code=503)

    log.info(f"TTS request: voice={_voice_name}, text_len={len(req.input)}, speed={req.speed}")

    chunks = preprocess_text(req.input)
    if not chunks:
        return Response(content="No text to synthesize", status_code=400)

    start = time.monotonic()

    # Piper's synthesize() yields AudioChunk objects with raw PCM16 data.
    # We collect them all and wrap in a WAV container at the end.
    #
    # length_scale controls speed: >1.0 = slower, <1.0 = faster.
    # This is the inverse of the OpenAI speed param, so we invert it.
    from piper.config import SynthesisConfig
    syn_config = SynthesisConfig(
        length_scale=1.0 / req.speed if req.speed > 0 else 1.0,
    )

    sample_rate = _voice.config.sample_rate
    pcm_parts = []

    for chunk in chunks:
        try:
            cleaned = clean_for_tts(chunk["text"])
            for audio_chunk in _voice.synthesize(cleaned, syn_config=syn_config):
                pcm_parts.append(audio_chunk.audio_int16_bytes)
        except Exception as e:
            log.error(f"TTS synthesis failed for chunk: {e}")
            return Response(content=str(e), status_code=500)

        if chunk["pause_after_ms"] > 0:
            pcm_parts.append(make_silence(chunk["pause_after_ms"], sample_rate))

    # Wrap raw PCM in WAV.
    pcm_data = b"".join(pcm_parts)
    buf = io.BytesIO()
    with wave.open(buf, "wb") as wf:
        wf.setnchannels(1)
        wf.setsampwidth(2)  # 16-bit
        wf.setframerate(sample_rate)
        wf.writeframes(pcm_data)
    wav_bytes = buf.getvalue()

    elapsed = time.monotonic() - start
    log.info(f"TTS complete: {len(chunks)} chunks, {len(wav_bytes)} bytes WAV in {elapsed:.2f}s")
    return Response(content=wav_bytes, media_type="audio/wav")


@app.get("/healthz")
async def health():
    return {"status": "ok", "engine": "piper", "voice": _voice_name}


def main():
    global _voice, _voice_name

    parser = argparse.ArgumentParser(description="Piper TTS server")
    parser.add_argument("--host", default="127.0.0.1")
    parser.add_argument("--port", type=int, default=8766)
    parser.add_argument(
        "--model",
        default=None,
        help="Path to .onnx voice model (default: scripts/piper-voices/en_GB-southern_english_female-low.onnx)",
    )
    args = parser.parse_args()

    # Resolve model path. Default to the southern_english_female voice.
    script_dir = Path(__file__).parent
    if args.model:
        model_path = Path(args.model)
    else:
        model_path = script_dir / "piper-voices" / "en_GB-southern_english_female-low.onnx"

    if not model_path.exists():
        log.error(f"Voice model not found: {model_path}")
        log.error("Run `her setup` to download voice models.")
        raise SystemExit(1)

    # The .onnx.json config must be next to the .onnx file.
    config_path = Path(str(model_path) + ".json")
    if not config_path.exists():
        log.error(f"Voice config not found: {config_path}")
        raise SystemExit(1)

    _voice_name = model_path.stem
    log.info(f"Loading voice: {_voice_name}")

    from piper import PiperVoice
    _voice = PiperVoice.load(str(model_path))
    log.info(f"Voice loaded (sample_rate={_voice.config.sample_rate}), starting server")

    uvicorn.run(app, host=args.host, port=args.port, log_level="info")


if __name__ == "__main__":
    main()
