<script lang="ts">
	import { tick } from 'svelte';
	import { MAX_CHAT_CHARS, type ChatMessage } from './chat';
	import { colorFor } from './presence';

	type Props = {
		messages: ChatMessage[];
		/** Older messages can be loaded. */
		more: boolean;
		/** "Ayşe is typing…" or ''. */
		typing: string;
		clientId: string;
		/** Why this member cannot write, or '' if they can. */
		blocked: string;
		/** The selected board object and a short label for it, if any. */
		selection: { id: string; label: string } | null;
		/** Label for a referenced object, or null once it is deleted. */
		describe: (ref: string) => string | null;
		onsend: (text: string, ref?: string) => boolean;
		ontyping: () => void;
		onolder: () => Promise<void>;
		onfocus: (ref: string) => void;
		onclose: () => void;
	};
	let { messages, more, typing, clientId, blocked, selection, describe, onsend, ontyping, onolder, onfocus, onclose }: Props =
		$props();

	let text = $state('');
	let attach = $state(true);
	let loadingOlder = $state(false);
	let list: HTMLOListElement;
	let input = $state<HTMLTextAreaElement>();

	const time = new Intl.DateTimeFormat(undefined, { hour: '2-digit', minute: '2-digit' });

	// Stick to the bottom when a message arrives and the reader was already
	// there; leave them alone while they scroll back through history.
	let stuck = true;
	let lastId = 0;
	$effect.pre(() => {
		const newest = messages[messages.length - 1]?.id ?? 0;
		if (newest === lastId) return;
		lastId = newest;
		if (list) stuck = list.scrollHeight - list.scrollTop - list.clientHeight < 48;
		tick().then(() => {
			if (stuck && list) list.scrollTop = list.scrollHeight;
		});
	});

	async function older() {
		if (loadingOlder) return;
		loadingOlder = true;
		const before = list.scrollHeight - list.scrollTop;
		try {
			await onolder();
			await tick();
			list.scrollTop = list.scrollHeight - before; // keep the view where it was
		} finally {
			loadingOlder = false;
		}
	}

	function submit(e?: Event) {
		e?.preventDefault();
		const t = text.trim();
		if (!t || t.length > MAX_CHAT_CHARS) return;
		const ref = attach && selection ? selection.id : undefined;
		if (onsend(t, ref)) {
			text = '';
			// One message about the selection is usually enough; the box
			// ticks itself again when something else is selected.
			if (ref) attach = false;
			stuck = true;
			input?.focus();
		}
	}

	function onkeydown(e: KeyboardEvent) {
		if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) submit(e);
		else if (e.key === 'Escape') onclose();
	}

	// Pick attaching back up whenever a different object is selected.
	$effect(() => {
		void selection?.id;
		attach = true;
	});
</script>

<aside class="chat" aria-label="Room chat">
	<header>
		<strong>Chat</strong>
		<button type="button" class="close" onclick={onclose} aria-label="Close chat">×</button>
	</header>
	<ol bind:this={list} class="messages" aria-live="polite">
		{#if more}
			<li class="older">
				<button type="button" onclick={older} disabled={loadingOlder}>{loadingOlder ? 'Loading…' : 'Load older messages'}</button>
			</li>
		{:else if messages.length === 0}
			<li class="empty">No messages yet. Say hi!</li>
		{/if}
		{#each messages as m (m.id)}
			{@const mine = m.from === clientId}
			{@const label = m.ref ? describe(m.ref) : null}
			<li class:mine data-chat-id={m.id}>
				<div class="meta">
					<span class="who" style:--c={colorFor(m.from)}>{mine ? 'You' : m.name || 'Guest'}</span>
					<time datetime={m.at}>{time.format(new Date(m.at))}</time>
				</div>
				<p class="text">{m.text}</p>
				{#if m.ref}
					{#if label}
						<button type="button" class="ref" onclick={() => onfocus(m.ref!)} title="Show on the board">{label}</button>
					{:else}
						<span class="ref gone">Deleted object</span>
					{/if}
				{/if}
			</li>
		{/each}
	</ol>
	<p class="typing" aria-live="polite">{typing}</p>
	{#if blocked}
		<p class="blocked">{blocked}</p>
	{:else}
		<form onsubmit={submit}>
			{#if selection}
				<label class="attach">
					<input type="checkbox" bind:checked={attach} />
					About: <span>{selection.label}</span>
				</label>
			{/if}
			<div class="row">
				<textarea
					bind:this={input}
					bind:value={text}
					{onkeydown}
					oninput={() => text.trim() && ontyping()}
					rows="1"
					maxlength={MAX_CHAT_CHARS}
					placeholder="Message the room"
					aria-label="Message"
				></textarea>
				<button type="submit" disabled={!text.trim()}>Send</button>
			</div>
			{#if text.length > MAX_CHAT_CHARS - 100}<small>{text.length}/{MAX_CHAT_CHARS}</small>{/if}
		</form>
	{/if}
</aside>

<style>
	.chat {
		display: flex;
		flex-direction: column;
		width: 100%;
		height: 100%;
		background: white;
		border-left: 1px solid #e4e4e7;
		font-size: 0.9rem;
	}
	header {
		display: flex;
		align-items: center;
		justify-content: space-between;
		padding: 0.5rem 0.75rem;
		border-bottom: 1px solid #f4f4f5;
	}
	.close {
		border: 0;
		background: none;
		font-size: 1.3rem;
		line-height: 1;
		cursor: pointer;
		color: #71717a;
	}
	.messages {
		flex: 1;
		min-height: 0;
		overflow-y: auto;
		margin: 0;
		padding: 0.5rem 0.75rem;
		list-style: none;
		display: flex;
		flex-direction: column;
		gap: 0.6rem;
	}
	.messages li {
		max-width: 88%;
	}
	.messages li.mine {
		align-self: flex-end;
		text-align: right;
	}
	.meta {
		display: flex;
		gap: 0.4rem;
		align-items: baseline;
		font-size: 0.75rem;
		color: #71717a;
	}
	.mine .meta {
		justify-content: flex-end;
	}
	.who {
		font-weight: 600;
		color: var(--c);
	}
	.text {
		margin: 0.15rem 0 0;
		padding: 0.4rem 0.6rem;
		border-radius: 10px;
		background: #f4f4f5;
		white-space: pre-wrap;
		overflow-wrap: anywhere;
		text-align: left;
	}
	.mine .text {
		background: #18181b;
		color: white;
	}
	.ref {
		margin-top: 0.2rem;
		padding: 0.1rem 0.45rem;
		border: 1px solid #c7d2fe;
		border-radius: 999px;
		background: #eef2ff;
		color: #3730a3;
		font: inherit;
		font-size: 0.75rem;
		cursor: pointer;
	}
	.ref.gone {
		display: inline-block;
		border-color: #e4e4e7;
		background: none;
		color: #a1a1aa;
		cursor: default;
	}
	.older,
	.empty {
		align-self: center;
		color: #71717a;
	}
	.older button {
		border: 0;
		background: none;
		color: #2563eb;
		font: inherit;
		cursor: pointer;
	}
	.typing {
		min-height: 1.1rem;
		margin: 0;
		padding: 0 0.75rem;
		font-size: 0.75rem;
		color: #71717a;
	}
	.blocked {
		margin: 0;
		padding: 0.75rem;
		border-top: 1px solid #f4f4f5;
		color: #71717a;
		text-align: center;
	}
	form {
		display: grid;
		gap: 0.35rem;
		padding: 0.5rem 0.75rem 0.75rem;
		border-top: 1px solid #f4f4f5;
	}
	.attach {
		display: flex;
		align-items: center;
		gap: 0.35rem;
		font-size: 0.75rem;
		color: #52525b;
	}
	.attach span {
		overflow: hidden;
		white-space: nowrap;
		text-overflow: ellipsis;
		font-weight: 600;
	}
	.row {
		display: flex;
		gap: 0.4rem;
		align-items: flex-end;
	}
	textarea {
		flex: 1;
		min-height: 2.2rem;
		max-height: 8rem;
		padding: 0.45rem 0.55rem;
		border: 1px solid #d4d4d8;
		border-radius: 8px;
		font: inherit;
		resize: none;
		field-sizing: content;
	}
	.row button {
		padding: 0.45rem 0.8rem;
		border: 0;
		border-radius: 8px;
		background: #18181b;
		color: white;
		font: inherit;
		cursor: pointer;
	}
	.row button:disabled {
		opacity: 0.4;
		cursor: default;
	}
	small {
		justify-self: end;
		color: #b45309;
	}
</style>
