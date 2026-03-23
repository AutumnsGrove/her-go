#!/usr/bin/env python3
"""Minimal TTS server for Kokoro via mlx-audio.

Exposes a single POST /v1/audio/speech endpoint (OpenAI-compatible).
Accepts JSON with text + voice, returns WAV audio bytes.

Usage:
    uv run scripts/tts_server.py                          # defaults
    uv run scripts/tts_server.py --port 8766 --model /path/to/model

This exists because mlx-audio's built-in server has broken dependency
management. This script uses mlx-audio as a library only.
"""
# /// script
# requires-python = ">=3.11"
# dependencies = [
#     "mlx-audio>=0.4",
#     "misaki==0.6.4",
#     "num2words",
#     "fastapi>=0.100",
#     "uvicorn>=0.22",
# ]
# ///

import argparse
import io
import logging
import wave

import numpy as np
import uvicorn
from fastapi import FastAPI
from fastapi.responses import Response
from pydantic import BaseModel

logging.basicConfig(level=logging.INFO)
log = logging.getLogger("tts-server")

app = FastAPI(title="Kokoro TTS Server")

# Global model reference — loaded once at startup.
_model = None
_model_name = None


class SpeechRequest(BaseModel):
    model: str = ""
    input: str
    voice: str = "af_heart"
    speed: float = 1.0
    response_format: str = "wav"


def audio_to_wav(audio_np: np.ndarray, sample_rate: int = 24000) -> bytes:
    """Convert float32 audio array to 16-bit PCM WAV bytes."""
    buf = io.BytesIO()
    pcm = (audio_np * 32767).clip(-32768, 32767).astype(np.int16)
    with wave.open(buf, "wb") as wf:
        wf.setnchannels(1)
        wf.setsampwidth(2)
        wf.setframerate(sample_rate)
        wf.writeframes(pcm.tobytes())
    return buf.getvalue()


@app.post("/v1/audio/speech")
async def speech(req: SpeechRequest):
    if _model is None:
        return Response(content="Model not loaded", status_code=503)

    log.info(f"TTS request: voice={req.voice}, text_len={len(req.input)}")

    # Infer lang_code from voice prefix.
    # Voice IDs follow the pattern: [lang][gender]_name
    # a=American, b=British, j=Japanese, z=Mandarin, etc.
    lang_code = req.voice[0] if req.voice else "a"

    chunks = []
    for result in _model.generate(
        text=req.input,
        voice=req.voice,
        speed=req.speed,
        lang_code=lang_code,
    ):
        chunks.append(np.array(result.audio))

    if not chunks:
        return Response(content="No audio generated", status_code=500)

    audio = np.concatenate(chunks).astype(np.float32)
    wav_bytes = audio_to_wav(audio)

    log.info(f"TTS complete: {len(wav_bytes)} bytes WAV")
    return Response(content=wav_bytes, media_type="audio/wav")


@app.get("/healthz")
async def health():
    return {"status": "ok", "model": _model_name}


def main():
    global _model, _model_name

    parser = argparse.ArgumentParser(description="Kokoro TTS server")
    parser.add_argument("--host", default="127.0.0.1")
    parser.add_argument("--port", type=int, default=8766)
    parser.add_argument("--model", default="mlx-community/Kokoro-82M-bf16",
                        help="HuggingFace model ID or local path")
    args = parser.parse_args()

    _model_name = args.model
    log.info(f"Loading model: {args.model}")

    from mlx_audio.tts.utils import load_model
    _model = load_model(args.model)
    log.info("Model loaded, starting server")

    uvicorn.run(app, host=args.host, port=args.port, log_level="info")


if __name__ == "__main__":
    main()
