"""
Gradio chat frontend for her-go gateway mode.

Connects to the Go backend's HTTP API (gateway gradio adapter) and
provides a browser-based chat interface with a live agent trace panel.

Usage:
    # Terminal 1: start the Go backend with gradio adapter enabled
    her run

    # Terminal 2: start the Gradio frontend
    uv run scripts/dev_chat.py

    # Browser: open http://localhost:7860

Requirements (installed automatically by uv):
    pip install gradio requests sseclient-py
"""

# /// script
# requires-python = ">=3.11"
# dependencies = [
#     "gradio>=5.0",
#     "requests>=2.31",
#     "sseclient-py>=1.8",
# ]
# ///

import base64
import json
import mimetypes
import threading
from pathlib import Path

import requests
import sseclient
import gradio as gr

API_BASE = "http://localhost:7777"

conversation_id = None


def check_backend():
    """Verify the Go backend is running."""
    try:
        resp = requests.get(f"{API_BASE}/api/status", timeout=2)
        return resp.status_code == 200
    except requests.ConnectionError:
        return False


def chat(message: dict, history: list) -> str:
    """Send a message to the Go backend and return the reply.

    In multimodal mode, Gradio sends message as a dict with:
      - "text": the user's text
      - "files": list of uploaded file paths (images, etc.)
    """
    global conversation_id

    text = message.get("text", "") if isinstance(message, dict) else message
    files = message.get("files", []) if isinstance(message, dict) else []

    payload = {"message": text}
    if conversation_id:
        payload["conversation_id"] = conversation_id

    # Attach the first image if one was uploaded.
    if files:
        img_path = Path(files[0])
        if img_path.exists():
            with open(img_path, "rb") as f:
                payload["image_base64"] = base64.b64encode(f.read()).decode()
            payload["image_mime"] = mimetypes.guess_type(str(img_path))[0] or "image/png"

    try:
        resp = requests.post(
            f"{API_BASE}/api/chat",
            json=payload,
            timeout=120,
        )
        resp.raise_for_status()
    except requests.ConnectionError:
        return "Backend not running. Start it with: `her run`"
    except requests.Timeout:
        return "Request timed out — the agent may be processing a complex query."
    except requests.HTTPError as e:
        return f"Error: {e.response.status_code} — {e.response.text}"

    data = resp.json()
    conversation_id = data.get("conversation_id", conversation_id)
    return data.get("reply", "")


def clear_conversation():
    """Start a new conversation."""
    global conversation_id
    try:
        resp = requests.post(f"{API_BASE}/api/clear", timeout=5)
        data = resp.json()
        conversation_id = data.get("conversation_id")
    except requests.ConnectionError:
        pass
    return None


# --- Trace SSE listener ---
# Connects to the backend's /api/traces SSE stream and accumulates
# trace events into a list. The trace panel polls this list on a timer.

trace_log: list[str] = []
trace_lock = threading.Lock()


def format_trace_event(event_type: str, data: dict) -> str:
    """Format a structured bus event as a plain text trace line."""
    if event_type == "turn_start":
        msg = data.get("user_message", "")
        return f"─── turn start: {msg[:60]} ───"
    elif event_type == "agent_iter":
        return f"  tokens: {data.get('prompt_tokens', 0)}+{data.get('completion_tokens', 0)} | ${data.get('cost_usd', 0):.6f} | {data.get('finish_reason', '')}"
    elif event_type == "tool_call":
        src = data.get("source", "main")
        name = data.get("tool_name", "?")
        result = data.get("result", "")[:80]
        prefix = f"[{src}] " if src != "main" else ""
        if data.get("is_error"):
            return f"  {prefix}❌ {name} → {result}"
        if name == "think":
            args = data.get("args", "")
            try:
                args = json.loads(args).get("thought", args)
            except (json.JSONDecodeError, AttributeError):
                pass
            return f"  {prefix}🧠 {args[:100]}"
        return f"  {prefix}{name} → {result}"
    elif event_type == "reply":
        text = data.get("text", "")[:80]
        return f"  💬 {data.get('total_tokens', 0)} tokens | ${data.get('cost_usd', 0):.6f} | {text}"
    elif event_type == "turn_end":
        return f"─── done: ${data.get('total_cost', 0):.4f} | {data.get('elapsed_ms', 0)}ms | {data.get('tool_calls', 0)} tools ───"
    elif event_type == "mood":
        labels = ", ".join(data.get("labels", []))
        return f"  🎭 {data.get('action', '')} v={data.get('valence', 0)} [{labels}]"
    elif event_type == "persona":
        return f"  🪞 {data.get('action', '')} {data.get('detail', '')}"
    elif event_type == "context":
        return "  📋 context ready"
    return f"  [{event_type}] {json.dumps(data)[:80]}"


def start_trace_listener():
    """Background thread that listens for structured SSE bus events."""
    while True:
        try:
            resp = requests.get(
                f"{API_BASE}/api/traces", stream=True, timeout=None
            )
            client = sseclient.SSEClient(resp)
            for event in client.events():
                try:
                    data = json.loads(event.data)
                    line = format_trace_event(event.event, data)
                    with trace_lock:
                        trace_log.append(line)
                        if len(trace_log) > 200:
                            del trace_log[:50]
                except (json.JSONDecodeError, KeyError):
                    pass
        except (requests.ConnectionError, requests.exceptions.ChunkedEncodingError):
            import time
            time.sleep(2)


def get_traces() -> str:
    """Return accumulated trace log as text for the panel."""
    with trace_lock:
        if not trace_log:
            return "Waiting for agent activity..."
        return "\n".join(trace_log[-100:])


# Start SSE listener in background
listener_thread = threading.Thread(target=start_trace_listener, daemon=True)
listener_thread.start()


# --- Build the Gradio UI ---

with gr.Blocks(title="her-go dev") as demo:
    gr.Markdown("## her-go dev chat")

    if not check_backend():
        gr.Markdown(
            "> **Backend not running.** Start it with `her run --adapter=gradio` "
            "or just `her run` with gradio enabled in config.yaml."
        )

    with gr.Row():
        # Left: chat panel (2/3 width)
        with gr.Column(scale=2):
            chatbot = gr.ChatInterface(
                fn=chat,
                multimodal=True,
                examples=[
                    {"text": "hey, how are you?"},
                    {"text": "what do you remember about me?"},
                ],
            )

        # Right: trace panel (1/3 width)
        with gr.Column(scale=1):
            gr.Markdown("### Agent Traces")
            trace_box = gr.Textbox(
                value=get_traces,
                label="Live Traces",
                lines=30,
                max_lines=30,
                interactive=False,
                every=1,  # poll every 1 second
            )

if __name__ == "__main__":
    demo.launch(theme=gr.themes.Soft())
