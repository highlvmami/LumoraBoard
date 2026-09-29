<script lang="ts">
	import { onDestroy, onMount } from 'svelte';
	import { page } from '$app/state';
	import { checkHealth, type BackendStatus } from '$lib/health';
	import { BoardStore, sorted, type BoardObject, type Op, type Point } from '$lib/board';
	import { colorFor, displayName, Presence, throttle } from '$lib/presence';
	import { connectRoom, type ConnectionState, type RoomConnection } from '$lib/ws';
	import Whiteboard, { type RemoteCursor } from '$lib/Whiteboard.svelte';

	const NAME_KEY = 'lumora.name';
	const ROOM_RE = /^[A-Za-z0-9_-]{1,64}$/;

	let health = $state<BackendStatus>('checking');
	let room = $state('demo');
	let roomInput = $state('demo');
	let name = $state('');
	let socket = $state<ConnectionState>('closed');
	let detail = $state('');
	let notice = $state('');

	// Store and presence are plain objects; the ticks bump whenever they
	// change so the derived values below re-read them. Board and presence
	// tick separately so cursor traffic never re-sorts the board.
	let boardTick = $state(0);
	let presenceTick = $state(0);
	let store = newStore();
	let presence = new Presence();
	let conn: RoomConnection | null = null;

	function newStore() {
		return new BoardStore((e) => {
			if (e.type === 'rejected') flash(`Change rejected: ${e.reason}`);
			boardTick++;
		});
	}

	let objects = $derived.by((): BoardObject[] => {
		void boardTick;
		return sorted(store.view);
	});
	let clientId = $derived.by(() => {
		void boardTick;
		return store.clientId;
	});
	let members = $derived.by(() => {
		void presenceTick;
		return [...presence.members.values()];
	});
	let cursors = $derived.by((): RemoteCursor[] => {
		void presenceTick;
		return [...presence.cursors].map(([id, c]) => ({
			id,
			x: c.x,
			y: c.y,
			color: colorFor(id),
			name: displayName(presence.members.get(id), id)
		}));
	});

	let noticeTimer: ReturnType<typeof setTimeout> | undefined;
	function flash(msg: string) {
		notice = msg;
		clearTimeout(noticeTimer);
		noticeTimer = setTimeout(() => (notice = ''), 4000);
	}

	onMount(() => {
		try {
			name = localStorage.getItem(NAME_KEY) ?? '';
		} catch {
			// Storage can be unavailable (private mode); a guest name is fine.
		}
		const fromUrl = page.url.searchParams.get('room');
		if (fromUrl && ROOM_RE.test(fromUrl)) room = roomInput = fromUrl;
		connect();
		checkHealth().then((h) => (health = h));
	});
	onDestroy(() => {
		conn?.close();
		sendCursor.cancel();
	});

	function connect() {
		conn?.close();
		store = newStore();
		presence = new Presence();
		notice = '';
		boardTick++;
		presenceTick++;
		conn = connectRoom(
			room,
			{
				onMessage: (env) => {
					if (env.type === 'cursor' || env.type === 'joined' || env.type === 'left') {
						if (presence.receive(env)) presenceTick++;
						return;
					}
					if (env.type === 'hello' && presence.receive(env)) presenceTick++;
					if (store.receive(env)) boardTick++;
				},
				onState: (s, d) => {
					socket = s;
					detail = d ?? '';
				}
			},
			{ since: () => store.seq, name: name.trim() }
		);
	}

	function join(e: SubmitEvent) {
		e.preventDefault();
		if (!ROOM_RE.test(roomInput)) return;
		room = roomInput;
		try {
			localStorage.setItem(NAME_KEY, name.trim());
		} catch {
			// See onMount.
		}
		const url = new URL(page.url);
		url.searchParams.set('room', room);
		history.replaceState(history.state, '', url);
		connect();
	}

	function send(op: Op) {
		if (!conn) return;
		try {
			const env = conn.send(op);
			store.local(env.clientOpId!, op);
			boardTick++;
		} catch {
			flash('Not connected; change not sent.');
		}
	}

	// ~25 Hz: smooth enough with the CSS glide, cheap for the room.
	const sendCursor = throttle((p: Point | null) => {
		conn?.sendCursor(p ? { x: Math.round(p.x * 10) / 10, y: Math.round(p.y * 10) / 10 } : { hidden: true });
	}, 40);

	function copyLink() {
		navigator.clipboard?.writeText(page.url.href).then(
			() => flash('Link copied.'),
			() => flash(page.url.href)
		);
	}
</script>

<svelte:head>
	<title>{room} · LumoraBoard</title>
</svelte:head>

<div class="app">
	<header>
		<strong class="logo">LumoraBoard</strong>
		<form onsubmit={join}>
			<input bind:value={roomInput} pattern={'[A-Za-z0-9_\\-]{1,64}'} required aria-label="Room" placeholder="room" />
			<input bind:value={name} maxlength="32" aria-label="Your name" placeholder="your name" />
			<button type="submit">Join</button>
		</form>
		<div class="people" aria-label="People in this room">
			<span class="avatar me" style:--c={colorFor(clientId || 'me')} title="{name || 'You'} (you)">
				{(name || 'You').slice(0, 1).toUpperCase()}
			</span>
			{#each members as m (m.id)}
				{@const label = displayName(m, m.id)}
				<span class="avatar" style:--c={colorFor(m.id)} title={label} data-member={m.id}>
					{label.replace('Guest ', '').slice(0, 1).toUpperCase()}
				</span>
			{/each}
		</div>
		<button type="button" class="share" onclick={copyLink}>Share</button>
		<p class="status">
			<span class="dot" data-status={socket}></span>
			<strong data-status={socket}>{socket}</strong>
			{#if detail && socket !== 'open'}<span class="muted">({detail})</span>{/if}
			<span class="muted">· backend <strong data-status={health}>{health}</strong></span>
		</p>
	</header>

	<main>
		<Whiteboard {objects} {cursors} {clientId} editable={socket === 'open'} onop={send} oncursor={(p) => sendCursor.call(p)} />
		{#if notice}<p class="notice" role="status">{notice}</p>{/if}
		{#if socket === 'reconnecting'}<p class="banner">Connection lost, reconnecting…</p>{/if}
	</main>
</div>

<style>
	:global(html, body) {
		margin: 0;
		height: 100%;
		font-family: system-ui, -apple-system, 'Segoe UI', sans-serif;
		color: #18181b;
		background: #fafafa;
	}
	.app {
		display: flex;
		flex-direction: column;
		height: 100dvh;
	}
	header {
		display: flex;
		align-items: center;
		flex-wrap: wrap;
		gap: 0.5rem 1rem;
		padding: 0.5rem 1rem;
		background: white;
		border-bottom: 1px solid #e4e4e7;
	}
	.logo {
		font-size: 1rem;
		letter-spacing: -0.01em;
	}
	form {
		display: flex;
		gap: 0.4rem;
	}
	input {
		width: 8rem;
		padding: 0.35rem 0.5rem;
		border: 1px solid #d4d4d8;
		border-radius: 6px;
		font: inherit;
		font-size: 0.9rem;
	}
	header button {
		padding: 0.35rem 0.75rem;
		border: 1px solid #d4d4d8;
		border-radius: 6px;
		background: white;
		font: inherit;
		font-size: 0.9rem;
		cursor: pointer;
	}
	.people {
		display: flex;
		margin-left: auto;
	}
	.avatar {
		display: grid;
		place-items: center;
		width: 28px;
		height: 28px;
		margin-left: -6px;
		border: 2px solid white;
		border-radius: 50%;
		background: var(--c);
		color: white;
		font-size: 0.75rem;
		font-weight: 600;
	}
	.status {
		display: flex;
		align-items: center;
		gap: 0.35rem;
		margin: 0;
		font-size: 0.8rem;
	}
	.status .dot {
		width: 8px;
		height: 8px;
		border-radius: 50%;
		background: currentColor;
	}
	.muted {
		color: #71717a;
	}
	main {
		position: relative;
		flex: 1;
		min-height: 0;
	}
	.notice,
	.banner {
		position: absolute;
		left: 50%;
		transform: translateX(-50%);
		margin: 0;
		padding: 0.4rem 0.8rem;
		border-radius: 6px;
		font-size: 0.85rem;
	}
	.notice {
		bottom: 16px;
		background: #18181b;
		color: white;
	}
	.banner {
		top: 64px;
		background: #fef3c7;
		color: #92400e;
	}
	[data-status='ok'],
	[data-status='open'] {
		color: #15803d;
	}
	[data-status='connecting'],
	[data-status='reconnecting'],
	[data-status='checking'] {
		color: #b45309;
	}
	[data-status='down'],
	[data-status='closed'] {
		color: #b91c1c;
	}
	@media (max-width: 640px) {
		header {
			padding: 0.4rem 0.75rem;
		}
		input {
			width: 6rem;
		}
		.status .muted {
			display: none;
		}
	}
</style>
