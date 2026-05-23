"""
Gradio chat frontend for her-go dev mode.

Connects to the Go backend's HTTP API (her dev) and provides a
browser-based chat interface with message bubbles.

Usage:
    # Terminal 1: start the Go backend
    her dev

    # Terminal 2: start the Gradio frontend
    uv run scripts/dev_chat.py

    # Browser: open http://localhost:7860

Requirements (installed automatically by uv):
    pip install gradio requests
"""

# /// script
# requires-python = ">=3.11"
# dependencies = [
#     "gradio>=5.0",
#     "requests>=2.31",
# ]
# ///

import requests
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
        return "Backend not running. Start it with: `her dev`"
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


# --- Build the Gradio UI ---

with gr.Blocks(title="her-go dev", theme=gr.themes.Soft()) as demo:
    gr.Markdown("## her-go dev chat")

    if not check_backend():
        gr.Markdown(
            "> **Backend not running.** Start it with `her dev` in another terminal."
        )

    chatbot = gr.ChatInterface(
        fn=chat,
        type="messages",
        examples=["hey, how are you?", "what do you remember about me?"],
    )

if __name__ == "__main__":
    demo.launch()
