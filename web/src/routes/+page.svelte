<script lang="ts">
	import { onMount, tick } from 'svelte';
	import { ChatSocket } from '$lib/ws';
	import {
		messages,
		connected,
		typing,
		status,
		addUserMessage,
		startAssistantMessage,
		appendToken,
		finishStream,
		setReply
	} from '$lib/stores';

	let input = $state('');
	let messagesEl: HTMLDivElement;
	let inputEl: HTMLTextAreaElement;
	let socket: ChatSocket;

	const WS_URL = 'ws://localhost:7778/ws';

	onMount(() => {
		socket = new ChatSocket(WS_URL, {
			onConnect: () => connected.set(true),
			onDisconnect: () => {
				connected.set(false);
				typing.set(false);
				status.set('');
			},
			onStreamToken: (token) => {
				appendToken(token);
				scrollToBottom();
			},
			onStreamEnd: () => {
				finishStream();
				typing.set(false);
				status.set('');
			},
			onReply: (text) => {
				setReply(text);
				typing.set(false);
				status.set('');
				scrollToBottom();
			},
			onStatus: (text) => {
				if (text.length > 50 && !text.endsWith('...')) {
					setReply(text);
					typing.set(false);
					status.set('');
				} else {
					status.set(text);
				}
				scrollToBottom();
			},
			onTyping: (active) => typing.set(active),
			onTrace: () => {},
			onError: (text) => {
				setReply(text);
				typing.set(false);
				status.set('');
			}
		});

		socket.connect();
		return () => socket.disconnect();
	});

	async function scrollToBottom() {
		await tick();
		if (messagesEl) messagesEl.scrollTop = messagesEl.scrollHeight;
	}

	function send() {
		const text = input.trim();
		if (!text || !$connected) return;

		addUserMessage(text);
		startAssistantMessage();
		socket.send(text);
		input = '';
		typing.set(true);
		scrollToBottom();

		if (inputEl) {
			inputEl.style.height = 'auto';
			inputEl.focus();
		}
	}

	function handleKeydown(e: KeyboardEvent) {
		if (e.key === 'Enter' && !e.shiftKey) {
			e.preventDefault();
			send();
		}
	}

	function autoResize(e: Event) {
		const el = e.target as HTMLTextAreaElement;
		el.style.height = 'auto';
		el.style.height = Math.min(el.scrollHeight, 150) + 'px';
	}
</script>

<div class="app">
	<header>
		<div class="header-left">
			<h1>mira</h1>
		</div>
		<div class="header-right">
			<span class="connection-dot" class:online={$connected}></span>
			<span class="connection-text">{$connected ? 'connected' : 'reconnecting...'}</span>
		</div>
	</header>

	<div class="messages" bind:this={messagesEl}>
		{#each $messages as msg (msg.id)}
			<div class="message {msg.role}">
				<div class="bubble">
					{#if msg.streaming && msg.text === ''}
						<span class="cursor-blink">|</span>
					{:else}
						{msg.text}{#if msg.streaming}<span class="cursor-blink">|</span>{/if}
					{/if}
				</div>
			</div>
		{/each}

		{#if $status && !$messages.some((m) => m.streaming)}
			<div class="status-indicator">{$status}</div>
		{/if}
	</div>

	<div class="input-area">
		<div class="input-row">
			<textarea
				bind:this={inputEl}
				bind:value={input}
				onkeydown={handleKeydown}
				oninput={autoResize}
				placeholder={$connected ? 'say something...' : 'connecting...'}
				disabled={!$connected}
				rows={1}
			></textarea>
			<button onclick={send} disabled={!$connected || !input.trim()}>
				<svg viewBox="0 0 24 24" width="20" height="20" fill="none" stroke="currentColor" stroke-width="2">
					<path d="M22 2L11 13M22 2l-7 20-4-9-9-4 20-7z" />
				</svg>
			</button>
		</div>
	</div>
</div>

<style>
	:global(*) {
		margin: 0;
		padding: 0;
		box-sizing: border-box;
	}

	:global(body) {
		font-family: 'Inter', -apple-system, BlinkMacSystemFont, sans-serif;
		background: #0a0a0f;
		color: #e2e2e8;
		overflow: hidden;
		height: 100dvh;
	}

	.app {
		display: flex;
		flex-direction: column;
		height: 100dvh;
		max-width: 720px;
		margin: 0 auto;
	}

	header {
		display: flex;
		align-items: center;
		justify-content: space-between;
		padding: 16px 20px;
		border-bottom: 1px solid rgba(255, 255, 255, 0.06);
	}

	h1 {
		font-size: 18px;
		font-weight: 600;
		letter-spacing: 0.5px;
		background: linear-gradient(135deg, #a78bfa, #818cf8);
		-webkit-background-clip: text;
		-webkit-text-fill-color: transparent;
	}

	.header-right {
		display: flex;
		align-items: center;
		gap: 8px;
	}

	.connection-dot {
		width: 8px;
		height: 8px;
		border-radius: 50%;
		background: #ef4444;
		transition: background 0.3s;
	}

	.connection-dot.online {
		background: #22c55e;
	}

	.connection-text {
		font-size: 12px;
		color: #6b7280;
	}

	.messages {
		flex: 1;
		overflow-y: auto;
		padding: 20px;
		display: flex;
		flex-direction: column;
		gap: 12px;
		scroll-behavior: smooth;
	}

	.messages::-webkit-scrollbar { width: 4px; }
	.messages::-webkit-scrollbar-thumb {
		background: rgba(255, 255, 255, 0.1);
		border-radius: 2px;
	}

	.message {
		display: flex;
		max-width: 85%;
	}

	.message.user { align-self: flex-end; }
	.message.assistant { align-self: flex-start; }

	.bubble {
		padding: 10px 16px;
		border-radius: 18px;
		line-height: 1.5;
		font-size: 15px;
		white-space: pre-wrap;
		word-wrap: break-word;
	}

	.message.user .bubble {
		background: #4f46e5;
		color: #f0f0ff;
		border-bottom-right-radius: 6px;
	}

	.message.assistant .bubble {
		background: rgba(255, 255, 255, 0.08);
		color: #d4d4dc;
		border-bottom-left-radius: 6px;
	}

	.cursor-blink {
		animation: blink 0.8s step-end infinite;
		color: #818cf8;
		font-weight: 300;
	}

	@keyframes blink {
		0%, 100% { opacity: 1; }
		50% { opacity: 0; }
	}

	.status-indicator {
		align-self: flex-start;
		font-size: 13px;
		color: #6b7280;
		padding: 4px 12px;
		font-style: italic;
	}

	.input-area {
		padding: 12px 20px 24px;
		border-top: 1px solid rgba(255, 255, 255, 0.06);
	}

	.input-row {
		display: flex;
		align-items: flex-end;
		gap: 10px;
		background: rgba(255, 255, 255, 0.06);
		border-radius: 24px;
		padding: 6px 6px 6px 18px;
		border: 1px solid rgba(255, 255, 255, 0.08);
		transition: border-color 0.2s;
	}

	.input-row:focus-within {
		border-color: rgba(129, 140, 248, 0.4);
	}

	textarea {
		flex: 1;
		background: none;
		border: none;
		color: #e2e2e8;
		font-family: inherit;
		font-size: 15px;
		resize: none;
		outline: none;
		padding: 8px 0;
		max-height: 150px;
		line-height: 1.4;
	}

	textarea::placeholder { color: #4b5563; }

	button {
		background: #4f46e5;
		color: white;
		border: none;
		border-radius: 50%;
		width: 36px;
		height: 36px;
		display: flex;
		align-items: center;
		justify-content: center;
		cursor: pointer;
		flex-shrink: 0;
		transition: background 0.2s, opacity 0.2s;
	}

	button:hover:not(:disabled) { background: #6366f1; }
	button:disabled { opacity: 0.3; cursor: not-allowed; }

	@media (max-width: 480px) {
		.app { max-width: 100%; }
		.messages { padding: 12px; }
		.message { max-width: 90%; }
		.input-area { padding: 8px 12px 16px; }
	}
</style>
