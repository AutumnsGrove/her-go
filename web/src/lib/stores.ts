import { writable, derived } from 'svelte/store';

export type Message = {
	id: string;
	role: 'user' | 'assistant';
	text: string;
	streaming: boolean;
	timestamp: Date;
};

export const messages = writable<Message[]>([]);
export const connected = writable(false);
export const typing = writable(false);
export const status = writable('');

export const messageCount = derived(messages, ($msgs) =>
	$msgs.filter((m) => m.role === 'user').length
);

let streamingId: string | null = null;

export function addUserMessage(text: string): string {
	const id = crypto.randomUUID();
	messages.update((msgs) => [
		...msgs,
		{ id, role: 'user', text, streaming: false, timestamp: new Date() }
	]);
	return id;
}

export function startAssistantMessage(): string {
	const id = crypto.randomUUID();
	streamingId = id;
	messages.update((msgs) => [
		...msgs,
		{ id, role: 'assistant', text: '', streaming: true, timestamp: new Date() }
	]);
	return id;
}

export function appendToken(token: string) {
	if (!streamingId) return;
	messages.update((msgs) =>
		msgs.map((m) => (m.id === streamingId ? { ...m, text: m.text + token } : m))
	);
}

export function finishStream() {
	if (!streamingId) return;
	messages.update((msgs) =>
		msgs.map((m) => (m.id === streamingId ? { ...m, streaming: false } : m))
	);
	streamingId = null;
}

export function setReply(text: string) {
	if (streamingId) {
		messages.update((msgs) =>
			msgs.map((m) =>
				m.id === streamingId ? { ...m, text, streaming: false } : m
			)
		);
		streamingId = null;
	} else {
		messages.update((msgs) => [
			...msgs,
			{ id: crypto.randomUUID(), role: 'assistant', text, streaming: false, timestamp: new Date() }
		]);
	}
}

export function clearMessages() {
	messages.set([]);
	streamingId = null;
}
