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

# Global voice reference -- loaded once at startup.
_voice = None
_voice_name = None

# Pause durations (ms) -- set from CLI args at startup, which come from
# config.yaml via the Go sidecar launcher. These defaults match
# config.yaml.example and are only used when running the server standalone.
PARAGRAPH_PAUSE_MS = 500
LINE_PAUSE_MS = 250
SENTENCE_PAUSE_MS = 75
COMMA_PAUSE_MS = 50
SEMI_PAUSE_MS = 30


class PauseConfig(BaseModel):
    """Per-request pause overrides. When present in the speech request,
    these take priority over the CLI defaults -- enabling hot-reload
    from config.yaml without restarting the sidecar."""
    paragraph_ms: int | None = None
    line_ms: int | None = None
    sentence_ms: int | None = None
    comma_ms: int | None = None
    semi_ms: int | None = None


class SpeechRequest(BaseModel):
    model: str = ""
    input: str
    voice: str = "en_GB-southern_english_female-low"
    speed: float = 1.0
    response_format: str = "wav"
    pauses: PauseConfig | None = None


def make_silence(duration_ms: int, sample_rate: int) -> bytes:
    """Create silent PCM16 bytes of the given duration."""
    n_samples = int(sample_rate * duration_ms / 1000)
    return b"\x00\x00" * n_samples


def clean_for_tts(text: str) -> str:
    """Clean text for better TTS pronunciation.

    Handles characters and words that Piper struggles with.
    """
    # All dash-like punctuation -> comma (which triggers a pause in the splitter).
    # Covers: em dash (—), en dash (–), double hyphen (--), spaced hyphen ( - ).
    # Without this, Piper blows right past dashes with no breath.
    text = re.sub(r"\s*[—–]\s*", ", ", text)
    text = re.sub(r"\s*--+\s*", ", ", text)
    text = re.sub(r"\s+-\s+", ", ", text)

    # "Huh" sounds robotic in Piper -- replace with a more natural filler.
    text = re.sub(r"\bhuh\b", "hmm", text, flags=re.IGNORECASE)

    # Ellipsis -> period (Piper handles sentence-end pauses better than ...)
    text = re.sub(r"\.{2,}", ".", text)
    return text


def _resolve_pauses(overrides):
    """Merge per-request pause overrides with CLI defaults.

    Returns a dict with all five pause keys, preferring request values
    when present (not None), falling back to the module-level defaults
    set at startup from CLI args.
    """
    return {
        "paragraph": overrides.paragraph_ms if overrides and overrides.paragraph_ms is not None else PARAGRAPH_PAUSE_MS,
        "line": overrides.line_ms if overrides and overrides.line_ms is not None else LINE_PAUSE_MS,
        "sentence": overrides.sentence_ms if overrides and overrides.sentence_ms is not None else SENTENCE_PAUSE_MS,
        "comma": overrides.comma_ms if overrides and overrides.comma_ms is not None else COMMA_PAUSE_MS,
        "semi": overrides.semi_ms if overrides and overrides.semi_ms is not None else SEMI_PAUSE_MS,
    }


# Regex: match text up to a punctuation mark. Group 1 = the fragment
# including punctuation, group 2 = the punctuation character itself.
_PUNCT_SPLIT = re.compile(r"(.*?([.!?;:,]))\s*")


def split_punctuation(text, pauses):
    """Split a single line into clause/sentence fragments with pause hints."""
    fragments = []
    pos = 0
    for m in _PUNCT_SPLIT.finditer(text):
        if m.start() < pos:
            continue
        fragment = m.group(1).strip()
        if not fragment:
            pos = m.end()
            continue

        punct = m.group(2)
        if punct in ".!?":
            pause = pauses["sentence"]
        elif punct in ";:":
            pause = pauses["semi"]
        else:
            pause = pauses["comma"]

        fragments.append({"text": fragment, "pause_after_ms": pause})
        pos = m.end()

    remainder = text[pos:].strip()
    if remainder:
        fragments.append({"text": remainder, "pause_after_ms": 0})

    if not fragments:
        fragments.append({"text": text.strip(), "pause_after_ms": 0})

    return fragments


def preprocess_text(text, pauses):
    """Split text into chunks with pause hints at every natural boundary.

    Hierarchy (outer to inner):
      1. Paragraph breaks -> pauses["paragraph"]
      2. Line breaks -> pauses["line"]
      3. Sentence-ending punctuation (. ! ?) -> pauses["sentence"]
      4. Clause-level punctuation (, ; :) -> pauses["comma"] / pauses["semi"]
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
                line_pause = 0
            elif is_last_line_in_para:
                line_pause = pauses["paragraph"]
            else:
                line_pause = pauses["line"]

            sub_chunks = split_punctuation(line, pauses)

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

    pauses = _resolve_pauses(req.pauses)
    log.info(f"TTS request: voice={_voice_name}, text_len={len(req.input)}, speed={req.speed}")

    chunks = preprocess_text(req.input, pauses)
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
        wf.setsampwidth(2)
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
        help="Path to .onnx voice model",
    )
    parser.add_argument("--pause-paragraph", type=int, default=500)
    parser.add_argument("--pause-line", type=int, default=250)
    parser.add_argument("--pause-sentence", type=int, default=75)
    parser.add_argument("--pause-comma", type=int, default=50)
    parser.add_argument("--pause-semi", type=int, default=30)
    args = parser.parse_args()

    global PARAGRAPH_PAUSE_MS, LINE_PAUSE_MS, SENTENCE_PAUSE_MS, COMMA_PAUSE_MS, SEMI_PAUSE_MS
    PARAGRAPH_PAUSE_MS = args.pause_paragraph
    LINE_PAUSE_MS = args.pause_line
    SENTENCE_PAUSE_MS = args.pause_sentence
    COMMA_PAUSE_MS = args.pause_comma
    SEMI_PAUSE_MS = args.pause_semi

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
