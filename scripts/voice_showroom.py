#!/usr/bin/env python3
"""Voice showroom — interactive Gradio UI for testing TTS and STT sidecars.

Launched via `her test voice`. Starts the sidecars if they're not already
running, then opens a browser with:
  - TTS tab: text input + 5 presets that exercise dynamic range
  - STT tab: microphone recording → transcription

Usage:
    uv run scripts/voice_showroom.py                     # defaults
    uv run scripts/voice_showroom.py --tts-url http://localhost:8766
    uv run scripts/voice_showroom.py --stt-url http://localhost:8765
"""
# /// script
# requires-python = ">=3.11,<3.14"
# dependencies = [
#     "gradio>=5.0",
#     "httpx",
# ]
# ///

import argparse
import subprocess
import sys
import time
from pathlib import Path

import gradio as gr
import httpx

DEFAULT_TTS_URL = "http://localhost:8766"
DEFAULT_STT_URL = "http://localhost:8765"

# Presets that exercise different prosodic features: punctuation pauses,
# clause breaks, emotional range, questions, and longer multi-sentence text.
TTS_PRESETS = {
    "Punctuation pauses": (
        "I went to the store. Then I came home. "
        "It was raining, so I grabbed an umbrella; the old one, not the new."
    ),
    "Questions & exclamations": (
        "Wait, really? You actually did that! "
        "I can't believe it. Are you sure? That's incredible!"
    ),
    "Soft & reflective": (
        "Sometimes, late at night, I think about the places we used to go. "
        "The park by the river. The little cafe on the corner. "
        "It feels like a lifetime ago."
    ),
    "Lists & colons": (
        "Here's what I need: eggs, flour, sugar, and butter. "
        "Oh, and one more thing: don't forget the vanilla extract."
    ),
    "Long paragraph": (
        "The morning light crept through the curtains, casting long golden "
        "stripes across the wooden floor. She sat by the window, cup of tea "
        "in hand, watching the birds gather on the fence outside. It was one "
        "of those rare, perfect mornings where everything felt still; not "
        "empty, but full of quiet. She smiled to herself, took a sip, and "
        "decided that today would be a good day."
    ),
}


def check_health(url: str) -> bool:
    """Check if a sidecar is responding."""
    try:
        r = httpx.get(f"{url}/healthz", timeout=2)
        return r.status_code == 200
    except Exception:
        return False


def start_tts_sidecar(tts_url: str, pause_args: list[str] | None = None) -> subprocess.Popen | None:
    """Start the Piper TTS server if not already running."""
    if check_health(tts_url):
        return None

    script = Path(__file__).parent / "tts_server.py"
    if not script.exists():
        return None

    # Parse host/port from URL
    from urllib.parse import urlparse
    parsed = urlparse(tts_url)
    host = parsed.hostname or "127.0.0.1"
    port = str(parsed.port or 8766)

    cmd = ["uv", "run", str(script), "--host", host, "--port", port]
    if pause_args:
        cmd.extend(pause_args)

    proc = subprocess.Popen(
        cmd,
        stdout=sys.stderr,
        stderr=sys.stderr,
    )

    # Wait for it to come up
    for _ in range(20):
        time.sleep(0.5)
        if check_health(tts_url):
            break

    return proc


def synthesize(text: str, speed: float, tts_url: str) -> str | None:
    """Send text to the TTS sidecar and return a path to the WAV file."""
    if not text.strip():
        return None

    try:
        r = httpx.post(
            f"{tts_url}/v1/audio/speech",
            json={
                "input": text,
                "voice": "en_GB-southern_english_female-low",
                "speed": speed,
                "response_format": "wav",
            },
            timeout=60,
        )
        r.raise_for_status()
    except Exception as e:
        raise gr.Error(f"TTS request failed: {e}")

    # Gradio audio component accepts a (sample_rate, numpy_array) tuple
    # or a file path. Easiest to write a temp file.
    import tempfile
    tmp = tempfile.NamedTemporaryFile(suffix=".wav", delete=False)
    tmp.write(r.content)
    tmp.close()
    return tmp.name


def transcribe(audio_path: str, stt_url: str) -> str:
    """Send recorded audio to the STT sidecar and return the transcription."""
    if audio_path is None:
        return ""

    with open(audio_path, "rb") as f:
        audio_bytes = f.read()

    try:
        r = httpx.post(
            f"{stt_url}/audio/transcriptions",
            files={"file": ("recording.wav", audio_bytes, "audio/wav")},
            data={"model": "mlx-community/parakeet-tdt-0.6b-v2-4bit"},
            timeout=120,
        )
        r.raise_for_status()
        return r.json().get("text", "(no text returned)")
    except Exception as e:
        raise gr.Error(f"STT request failed: {e}")


def build_app(tts_url: str, stt_url: str) -> gr.Blocks:
    """Build the Gradio interface."""
    theme = gr.themes.Base(
        primary_hue=gr.themes.colors.cyan,
        neutral_hue=gr.themes.colors.gray,
        font=[gr.themes.GoogleFont("IBM Plex Sans"), "system-ui", "sans-serif"],
        font_mono=[gr.themes.GoogleFont("IBM Plex Mono"), "monospace"],
    ).set(
        body_background_fill="*neutral_950",
        body_background_fill_dark="*neutral_950",
        block_background_fill="*neutral_900",
        block_background_fill_dark="*neutral_900",
        input_background_fill="*neutral_800",
        input_background_fill_dark="*neutral_800",
        body_text_color="*neutral_200",
        body_text_color_dark="*neutral_200",
        block_label_text_color="*neutral_400",
        block_label_text_color_dark="*neutral_400",
        border_color_primary="*neutral_700",
        border_color_primary_dark="*neutral_700",
    )
    with gr.Blocks(title="Voice Showroom", theme=theme) as app:
        gr.Markdown("# Voice Showroom\nTest TTS and STT sidecars interactively.")

        with gr.Tab("TTS — Text to Speech"):
            gr.Markdown("Type text or pick a preset, then hit **Speak**.")

            with gr.Row():
                preset_dropdown = gr.Dropdown(
                    choices=["(custom)"] + list(TTS_PRESETS.keys()),
                    value="(custom)",
                    label="Preset",
                    scale=1,
                )
                speed_slider = gr.Slider(
                    minimum=0.5, maximum=2.0, value=1.0, step=0.1,
                    label="Speed",
                    scale=1,
                )

            text_input = gr.Textbox(
                label="Text",
                lines=4,
                placeholder="Type something to speak...",
            )
            speak_btn = gr.Button("Speak", variant="primary")
            audio_output = gr.Audio(label="Output", type="filepath")

            # Wire up preset selection
            def on_preset(choice):
                if choice in TTS_PRESETS:
                    return TTS_PRESETS[choice]
                return ""

            preset_dropdown.change(on_preset, inputs=[preset_dropdown], outputs=[text_input])

            speak_btn.click(
                fn=lambda text, speed: synthesize(text, speed, tts_url),
                inputs=[text_input, speed_slider],
                outputs=[audio_output],
            )

        with gr.Tab("STT — Speech to Text"):
            gr.Markdown("Record yourself, then hit **Transcribe**.")

            audio_input = gr.Audio(
                label="Record",
                sources=["microphone"],
                type="filepath",
            )
            transcribe_btn = gr.Button("Transcribe", variant="primary")
            text_output = gr.Textbox(label="Transcription", lines=3)

            transcribe_btn.click(
                fn=lambda audio: transcribe(audio, stt_url),
                inputs=[audio_input],
                outputs=[text_output],
            )

        # Status footer
        tts_ok = check_health(tts_url)
        stt_ok = check_health(stt_url)
        status_parts = [
            f"TTS: {'ready' if tts_ok else 'offline'} ({tts_url})",
            f"STT: {'ready' if stt_ok else 'offline'} ({stt_url})",
        ]
        gr.Markdown(f"---\n`{' | '.join(status_parts)}`")

    return app


def main():
    parser = argparse.ArgumentParser(description="Voice showroom")
    parser.add_argument("--tts-url", default=DEFAULT_TTS_URL)
    parser.add_argument("--stt-url", default=DEFAULT_STT_URL)
    parser.add_argument("--port", type=int, default=7860)
    parser.add_argument("--pause-paragraph", type=int, default=0)
    parser.add_argument("--pause-line", type=int, default=0)
    parser.add_argument("--pause-sentence", type=int, default=0)
    parser.add_argument("--pause-comma", type=int, default=0)
    parser.add_argument("--pause-semi", type=int, default=0)
    args = parser.parse_args()

    # Forward pause config to the TTS sidecar if we need to start it.
    pause_args = []
    for name in ("paragraph", "line", "sentence", "comma", "semi"):
        val = getattr(args, f"pause_{name}")
        if val > 0:
            pause_args.extend([f"--pause-{name}", str(val)])

    # Auto-start TTS sidecar if needed
    tts_proc = start_tts_sidecar(args.tts_url, pause_args or None)
    if tts_proc:
        print(f"Started TTS sidecar (pid={tts_proc.pid})")

    try:
        app = build_app(args.tts_url, args.stt_url)
        app.launch(server_port=args.port, share=False)
    finally:
        if tts_proc and tts_proc.poll() is None:
            tts_proc.terminate()


if __name__ == "__main__":
    main()
