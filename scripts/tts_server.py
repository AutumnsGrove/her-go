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
# Duration of silence after sentence-ending punctuation (. ! ?).
SENTENCE_PAUSE_MS = 300
# Duration of silence after clause-level punctuation (, ; :).
CLAUSE_PAUSE_MS = 150


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


def split_punctuation(text: str) -> list[dict]:
    """Split a single line into clause/sentence fragments with pause hints.

    Splits at sentence-ending punctuation (. ! ?) and clause-level
    punctuation (, ; :) so that explicit silence can be inserted between
    them — Piper's low-quality models don't reliably pause at these marks.

    Closing quotes/parens that follow punctuation stay attached to the
    fragment (e.g. 'she said "hello!"' keeps the quote with "hello!").
    """
    # Regex: capture a chunk of text up to and including a punctuation mark,
    # plus any trailing quotes/parens/whitespace that belong with it.
    # Group 1 = the text fragment including its punctuation.
    # Group 2 = the punctuation character itself (for choosing pause length).
    pattern = re.compile(
        r'(.*?'           # non-greedy leading text
        r'([.!?;:,])'    # the punctuation mark we split on
        r'[\"\'\)\]”’]*'  # optional closing quotes/parens
        r')\s*'           # eat trailing whitespace
    )

    fragments = []
    pos = 0
    for m in pattern.finditer(text):
        # Skip if this match starts before our cursor (overlapping).
        if m.start() < pos:
            continue
        fragment = m.group(1).strip()
        if not fragment:
            pos = m.end()
            continue

        punct = m.group(2)
        if punct in ".!?":
            pause = SENTENCE_PAUSE_MS
        else:
            pause = CLAUSE_PAUSE_MS

        fragments.append({"text": fragment, "pause_after_ms": pause})
        pos = m.end()

    # Leftover text after the last punctuation mark (e.g. trailing clause
    # with no period). Append it with no pause — the line-level pause
    # from preprocess_text will handle the gap.
    remainder = text[pos:].strip()
    if remainder:
        fragments.append({"text": remainder, "pause_after_ms": 0})

    # If the text had no punctuation at all, return it as one chunk.
    if not fragments:
        fragments.append({"text": text.strip(), "pause_after_ms": 0})

    return fragments


def preprocess_text(text: str) -> list[dict]:
    """Split text into chunks with pause hints at every natural boundary.

    Hierarchy (outer to inner):
      1. Paragraph breaks (\\n\\n) → PARAGRAPH_PAUSE_MS
      2. Line breaks (\\n) → LINE_PAUSE_MS
      3. Sentence-ending punctuation (. ! ?) → SENTENCE_PAUSE_MS
      4. Clause-level punctuation (, ; :) → CLAUSE_PAUSE_MS

    Returns a list of dicts:
      {"text": "...", "pause_after_ms": <ms>}
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

            # Determine the pause that follows this entire line.
            if is_last_para and is_last_line_in_para:
                line_pause = 0
            elif is_last_line_in_para:
                line_pause = PARAGRAPH_PAUSE_MS
            else:
                line_pause = LINE_PAUSE_MS

            # Split the line at punctuation for intra-line pauses.
            sub_chunks = split_punctuation(line)

            # The last sub-chunk inherits the line-level pause (which is
            # always >= the punctuation pause), so structural pauses win.
            if sub_chunks:
                sub_chunks[-1]["pause_after_ms"] = max(
                    sub_chunks[-1]["pause_after_ms"], line_pause
                )

            chunks.extend(sub_chunks)

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
