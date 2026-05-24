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

import json
import threading

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


def chat(message: str, history: list) -> str:
    """Send a message to the Go backend and return the reply."""
    global conversation_id

    payload = {"message": message}
    if conversation_id:
        payload["conversation_id"] = conversation_id

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


def start_trace_listener():
    """Background thread that listens for SSE trace events."""
    while True:
        try:
            resp = requests.get(
                f"{API_BASE}/api/traces", stream=True, timeout=None
            )
            client = sseclient.SSEClient(resp)
            for event in client.events():
                try:
                    data = json.loads(event.data)
                    agent = data.get("Agent", "")
                    phase = data.get("Phase", "")
                    content = data.get("Content", "")
                    # Strip HTML tags for plain text display
                    import re
                    content = re.sub(r"<[^>]+>", "", content)
                    line = f"[{agent}/{phase}] {content}"
                    with trace_lock:
                        trace_log.append(line)
                        # Keep last 200 lines
                        if len(trace_log) > 200:
                            del trace_log[:50]
                except (json.JSONDecodeError, KeyError):
                    pass
        except (requests.ConnectionError, requests.exceptions.ChunkedEncodingError):
            import time
            time.sleep(2)  # reconnect after brief pause


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

with gr.Blocks(title="her-go dev", theme=gr.themes.Soft()) as demo:
    gr.Markdown("## her-go dev chat")

    if not check_backend():
        gr.Markdown(
            "> **Backend not running.** Start it with `her run` "
            "(with gradio adapter enabled in config.yaml)."
        )

    with gr.Row():
        # Left: chat panel (2/3 width)
        with gr.Column(scale=2):
            chatbot = gr.ChatInterface(
                fn=chat,
                type="messages",
                examples=[
                    "hey, how are you?",
                    "what do you remember about me?",
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
    demo.launch()
