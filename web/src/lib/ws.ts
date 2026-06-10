export type MessageHandler = {
	onReply: (text: string) => void;
	onStreamToken: (token: string) => void;
	onStreamEnd: () => void;
	onStatus: (text: string) => void;
	onTyping: (active: boolean) => void;
	onTrace: (eventType: string, data: unknown) => void;
	onError: (text: string) => void;
	onConnect: () => void;
	onDisconnect: () => void;
};

export class ChatSocket {
	private ws: WebSocket | null = null;
	private url: string;
	private handlers: MessageHandler;
	private reconnectTimer: ReturnType<typeof setTimeout> | null = null;
	private shouldReconnect = true;

	constructor(url: string, handlers: MessageHandler) {
		this.url = url;
		this.handlers = handlers;
	}

	connect() {
		if (this.ws?.readyState === WebSocket.OPEN) return;

		try {
			this.ws = new WebSocket(this.url);
		} catch {
			this.scheduleReconnect();
			return;
		}

		this.ws.onopen = () => {
			this.handlers.onConnect();
			if (this.reconnectTimer) {
				clearTimeout(this.reconnectTimer);
				this.reconnectTimer = null;
			}
		};

		this.ws.onclose = () => {
			this.handlers.onDisconnect();
			if (this.shouldReconnect) this.scheduleReconnect();
		};

		this.ws.onerror = () => {};

		this.ws.onmessage = (event) => {
			try {
				const msg = JSON.parse(event.data);
				this.dispatch(msg);
			} catch { /* ignore malformed */ }
		};
	}

	disconnect() {
		this.shouldReconnect = false;
		if (this.reconnectTimer) {
			clearTimeout(this.reconnectTimer);
			this.reconnectTimer = null;
		}
		this.ws?.close();
		this.ws = null;
	}

	send(text: string) {
		if (!this.ws || this.ws.readyState !== WebSocket.OPEN) return;
		this.ws.send(JSON.stringify({
			type: 'message',
			text,
			request_id: crypto.randomUUID()
		}));
	}

	get connected(): boolean {
		return this.ws?.readyState === WebSocket.OPEN;
	}

	private dispatch(msg: { type: string; [key: string]: unknown }) {
		switch (msg.type) {
			case 'reply': this.handlers.onReply(msg.text as string); break;
			case 'stream_token': this.handlers.onStreamToken(msg.token as string); break;
			case 'stream_end': this.handlers.onStreamEnd(); break;
			case 'status': this.handlers.onStatus(msg.text as string); break;
			case 'typing': this.handlers.onTyping(msg.active as boolean); break;
			case 'trace': this.handlers.onTrace(msg.event_type as string, msg.data); break;
			case 'error': this.handlers.onError(msg.text as string); break;
		}
	}

	private scheduleReconnect() {
		if (this.reconnectTimer) return;
		this.reconnectTimer = setTimeout(() => {
			this.reconnectTimer = null;
			this.connect();
		}, 3000);
	}
}
