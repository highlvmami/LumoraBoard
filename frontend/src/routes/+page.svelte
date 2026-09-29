<script lang="ts">
	import { onDestroy, onMount } from 'svelte';
	import { page } from '$app/state';
	import { replaceState } from '$app/navigation';
	import { checkHealth, type BackendStatus } from '$lib/health';
	import { BoardStore, canEdit, sorted, type BoardObject, type Op, type Point, type Role } from '$lib/board';
	import { colorFor, displayName, Presence, throttle } from '$lib/presence';
	import { CLOSE_FORBIDDEN, CLOSE_UNAUTHORIZED, connectRoom, type ConnectionState, type RoomConnection } from '$lib/ws';
	import { acceptInvite, createInvite, fetchAuthStatus, loginUrl, logout, type AuthStatus } from '$lib/auth';
	import Whiteboard, { type RemoteCursor } from '$lib/Whiteboard.svelte';
	import Chat from '$lib/Chat.svelte';
	import { ChatStore, fetchOlder, typingLabel } from '$lib/chat';

	const NAME_KEY = 'lumora.name';
	const ROOM_RE = /^[A-Za-z0-9_-]{1,64}$/;

	let health = $state<BackendStatus>('checking');
	let room = $state('demo');
	let roomInput = $state('demo');
	let name = $state('');
	let socket = $state<ConnectionState>('closed');
	let detail = $state('');
	let notice = $state('');
	/** Null until the server said how sign-in works. */
	let auth = $state<AuthStatus | null>(null);
	/** Why the board is not shown, if it is not. */
	let gate = $state<'' | 'signin' | 'forbidden'>('');
	let shareOpen = $state(false);
	let chatOpen = $state(false);
	let unread = $state(0);
	let selected = $state('');
	let wb = $state<ReturnType<typeof Whiteboard>>();

	// Store and presence are plain objects; the ticks bump whenever they
	// change so the derived values below re-read them. Board and presence
	// tick separately so cursor traffic never re-sorts the board.
	let boardTick = $state(0);
	let presenceTick = $state(0);
	let chatTick = $state(0);
	let store = newStore();
	let presence = new Presence();
	let chat = new ChatStore();
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
	let role = $derived.by((): Role => {
		void boardTick;
		return store.role;
	});
	let me = $derived(auth?.user?.name || name || 'You');
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

	let chatMessages = $derived.by(() => {
		void chatTick;
		return chat.messages;
	});
	let chatMore = $derived.by(() => {
		void chatTick;
		return chat.more;
	});
	let typingText = $derived.by(() => {
		void chatTick;
		void presenceTick;
		return typingLabel(chat.typers().map((id) => displayName(presence.members.get(id), id)));
	});
	let chatBlocked = $derived(auth?.enabled && !auth.user ? 'Sign in to join the chat.' : '');

	const KIND_LABEL: Record<string, string> = {
		stroke: 'Drawing',
		rect: 'Rectangle',
		ellipse: 'Ellipse',
		arrow: 'Arrow',
		text: 'Text',
		sticky: 'Sticky note'
	};

	/** Short label for a board object, or null if it no longer exists. */
	function describe(ref: string): string | null {
		const o = objects.find((x) => x.id === ref);
		if (!o) return null;
		const t = o.text?.trim().replace(/\s+/g, ' ');
		if (!t) return KIND_LABEL[o.kind] ?? o.kind;
		return `${KIND_LABEL[o.kind] ?? o.kind} “${t.length > 24 ? t.slice(0, 23) + '…' : t}”`;
	}

	let selection = $derived.by(() => {
		if (!selected) return null;
		const label = describe(selected);
		return label ? { id: selected, label } : null;
	});

	function sendChat(text: string, ref?: string): boolean {
		try {
			const env = conn!.sendChat(text, ref);
			chat.sent(env.clientOpId!);
			return true;
		} catch {
			flash('Not connected; message not sent.');
			return false;
		}
	}

	const sendTyping = throttle(() => conn?.sendTyping(), 1000);

	async function loadOlder() {
		const first = chat.messages[0];
		if (!first) return;
		try {
			chat.prepend(await fetchOlder(room, first.id));
			chatTick++;
		} catch (e) {
			flash(e instanceof Error ? e.message : String(e));
		}
	}

	function focusRef(ref: string) {
		if (!wb?.focus(ref)) {
			flash('That object was deleted.');
			return;
		}
		// On a phone the panel covers the board; get out of the way.
		if (matchMedia('(max-width: 640px)').matches) chatOpen = false;
	}

	function toggleChat() {
		chatOpen = !chatOpen;
		if (chatOpen) unread = 0;
	}

	// Typing notices expire on their own; re-read them while any are shown.
	const typingTimer = setInterval(() => {
		if (chat.typing.size > 0) chatTick++;
	}, 1000);

	let noticeTimer: ReturnType<typeof setTimeout> | undefined;
	function flash(msg: string) {
		notice = msg;
		clearTimeout(noticeTimer);
		noticeTimer = setTimeout(() => (notice = ''), 4000);
	}

	onMount(async () => {
		try {
			name = localStorage.getItem(NAME_KEY) ?? '';
		} catch {
			// Storage can be unavailable (private mode); a guest name is fine.
		}
		const fromUrl = page.url.searchParams.get('room');
		if (fromUrl && ROOM_RE.test(fromUrl)) room = roomInput = fromUrl;
		checkHealth().then((h) => (health = h));

		try {
			auth = await fetchAuthStatus();
		} catch {
			// An old or unreachable backend: behave as before sign-in existed.
			auth = { enabled: false, guests: false, providers: [] };
		}
		if (auth.enabled && !auth.user) {
			// Guests may watch; everyone else signs in first. An invite in
			// the URL survives the round trip through the provider.
			if (!auth.guests) {
				gate = 'signin';
				return;
			}
		}
		const joined = await redeemInvite();
		connect();
		if (joined) flash(joined);
	});

	/**
	 * Accepts ?invite=… once the user is signed in, then drops it from the
	 * URL. Returns a message for the user, if any.
	 */
	async function redeemInvite(): Promise<string> {
		const token = page.url.searchParams.get('invite');
		if (!token || !auth?.user) return '';
		let msg: string;
		try {
			const res = await acceptInvite(token);
			room = roomInput = res.board;
			msg = `You joined ${res.board} as ${res.role}.`;
		} catch (e) {
			msg = e instanceof Error ? e.message : String(e);
		}
		const url = new URL(page.url);
		url.searchParams.delete('invite');
		url.searchParams.set('room', room);
		replaceState(url, page.state);
		return msg;
	}

	onDestroy(() => {
		conn?.close();
		sendCursor.cancel();
		sendTyping.cancel();
		clearInterval(typingTimer);
	});

	function connect() {
		conn?.close();
		gate = '';
		store = newStore();
		presence = new Presence();
		chat = new ChatStore();
		unread = 0;
		selected = '';
		notice = '';
		boardTick++;
		presenceTick++;
		chatTick++;
		conn = connectRoom(
			room,
			{
				onMessage: (env) => {
					if (env.type === 'cursor' || env.type === 'joined' || env.type === 'left') {
						if (presence.receive(env)) presenceTick++;
						if (env.type === 'left' && chat.receive(env)) chatTick++;
						return;
					}
					if (env.type === 'chat.message' || env.type === 'chat.typing') {
						if (chat.receive(env)) chatTick++;
						if (env.type === 'chat.message' && !chatOpen && env.from !== store.clientId) unread++;
						return;
					}
					if (env.type === 'reject' && env.clientOpId && chat.claim(env.clientOpId)) {
						flash(`Message not sent: ${(env.payload as { reason?: string })?.reason ?? 'rejected'}`);
						return;
					}
					if (env.type === 'hello') {
						if (presence.receive(env)) presenceTick++;
						if (chat.receive(env)) chatTick++;
					}
					if (store.receive(env)) boardTick++;
				},
				onState: (s, d, code) => {
					socket = s;
					detail = d ?? '';
					if (code === CLOSE_UNAUTHORIZED) gate = 'signin';
					else if (code === CLOSE_FORBIDDEN) gate = 'forbidden';
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
		replaceState(url, page.state);
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

	function copy(text: string, what: string) {
		navigator.clipboard?.writeText(text).then(
			() => flash(`${what} copied.`),
			() => flash(text)
		);
	}

	async function invite(r: 'editor' | 'viewer') {
		shareOpen = false;
		try {
			copy(await createInvite(room, r), r === 'editor' ? 'Edit link' : 'View link');
		} catch (e) {
			flash(e instanceof Error ? e.message : String(e));
		}
	}

	async function signOut() {
		await logout();
		location.reload();
	}

	let here = $derived(page.url.pathname + page.url.search);
</script>

<svelte:head>
	<title>{room} · LumoraBoard</title>
</svelte:head>

<div class="app">
	<header>
		<strong class="logo">LumoraBoard</strong>
		<form onsubmit={join}>
			<input bind:value={roomInput} pattern={'[A-Za-z0-9_\\-]{1,64}'} required aria-label="Room" placeholder="room" />
			{#if !auth?.enabled}
				<input bind:value={name} maxlength="32" aria-label="Your name" placeholder="your name" />
			{/if}
			<button type="submit">Join</button>
		</form>
		<div class="people" aria-label="People in this room">
			<span class="avatar me" style:--c={colorFor(clientId || 'me')} title="{me} (you)">
				{#if auth?.user?.avatar}<img src={auth.user.avatar} alt="" />{:else}{me.slice(0, 1).toUpperCase()}{/if}
			</span>
			{#each members as m (m.id)}
				{@const label = displayName(m, m.id)}
				<span class="avatar" style:--c={colorFor(m.id)} title="{label}{m.role ? ` (${m.role})` : ''}" data-member={m.id}>
					{#if m.avatar}<img src={m.avatar} alt="" />{:else}{label.replace('Guest ', '').slice(0, 1).toUpperCase()}{/if}
				</span>
			{/each}
		</div>
		<button type="button" class="chat-toggle" aria-pressed={chatOpen} onclick={toggleChat}>
			Chat{#if unread > 0}<span class="badge" aria-label="{unread} unread">{unread > 99 ? '99+' : unread}</span>{/if}
		</button>
		<div class="share-wrap">
			{#if auth?.enabled && role === 'owner'}
				<button type="button" aria-expanded={shareOpen} onclick={() => (shareOpen = !shareOpen)}>Share</button>
				{#if shareOpen}
					<div class="menu" role="menu">
						<button type="button" role="menuitem" onclick={() => invite('editor')}>Copy edit link</button>
						<button type="button" role="menuitem" onclick={() => invite('viewer')}>Copy view-only link</button>
					</div>
				{/if}
			{:else}
				<button type="button" onclick={() => copy(page.url.href, 'Link')}>Share</button>
			{/if}
		</div>
		{#if auth?.user}
			<button type="button" class="quiet" onclick={signOut} title="Signed in as {auth.user.name}">Sign out</button>
		{:else if auth?.enabled && auth.guests}
			<button type="button" onclick={() => (gate = 'signin')}>Sign in to edit</button>
		{/if}
		<p class="status">
			<span class="dot" data-status={socket}></span>
			<strong data-status={socket}>{socket}</strong>
			{#if detail && socket !== 'open'}<span class="muted">({detail})</span>{/if}
			<span class="muted">· backend <strong data-status={health}>{health}</strong></span>
		</p>
	</header>

	<main>
		<div class="stage">
			{#if gate}
				<section class="gate">
					{#if gate === 'signin' && auth}
						<h1>Sign in to open “{room}”</h1>
						<p>LumoraBoard boards are private to the people their owner invites.</p>
						<div class="providers">
							{#each auth.providers as p (p.id)}
								<a class="button primary" href={loginUrl(p.id, here)}>Continue with {p.name}</a>
							{/each}
						</div>
					{:else}
						<h1>No access to “{room}”</h1>
						<p>Ask the board's owner for an invite link, or open another board from the box above.</p>
					{/if}
				</section>
			{:else}
				<Whiteboard
					bind:this={wb}
					bind:selected
					{objects}
					{cursors}
					{clientId}
					editable={socket === 'open' && canEdit(role)}
					readonly={!canEdit(role)}
					onop={send}
					oncursor={(p) => sendCursor.call(p)}
				/>
				{#if !canEdit(role) && socket === 'open'}<p class="viewonly">View only</p>{/if}
			{/if}
			{#if notice}<p class="notice" role="status">{notice}</p>{/if}
			{#if socket === 'reconnecting'}<p class="banner">Connection lost, reconnecting…</p>{/if}
		</div>
		{#if chatOpen && !gate}
			<div class="chat-pane">
				<Chat
					messages={chatMessages}
					more={chatMore}
					typing={typingText}
					{clientId}
					blocked={chatBlocked}
					{selection}
					{describe}
					onsend={sendChat}
					ontyping={() => sendTyping.call()}
					onolder={loadOlder}
					onfocus={focusRef}
					onclose={() => (chatOpen = false)}
				/>
			</div>
		{/if}
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
		display: flex;
		flex: 1;
		min-height: 0;
	}
	.stage {
		position: relative;
		flex: 1;
		min-width: 0;
	}
	.chat-pane {
		flex: none;
		width: 320px;
	}
	.chat-toggle {
		position: relative;
	}
	.chat-toggle[aria-pressed='true'] {
		background: #f4f4f5;
	}
	.badge {
		position: absolute;
		top: -6px;
		right: -6px;
		min-width: 18px;
		padding: 0 4px;
		border-radius: 999px;
		background: #e11d48;
		color: white;
		font-size: 0.7rem;
		line-height: 18px;
		text-align: center;
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
	.avatar img {
		width: 100%;
		height: 100%;
		border-radius: 50%;
		object-fit: cover;
	}
	.share-wrap {
		position: relative;
	}
	.menu {
		position: absolute;
		right: 0;
		top: calc(100% + 4px);
		z-index: 10;
		display: grid;
		min-width: 12rem;
		padding: 4px;
		background: white;
		border: 1px solid #e4e4e7;
		border-radius: 8px;
		box-shadow: 0 8px 24px rgba(0, 0, 0, 0.12);
	}
	.menu button {
		border: 0;
		text-align: left;
	}
	.menu button:hover {
		background: #f4f4f5;
	}
	.button {
		padding: 0.35rem 0.75rem;
		border: 1px solid #d4d4d8;
		border-radius: 6px;
		background: white;
		color: inherit;
		font-size: 0.9rem;
		text-decoration: none;
	}
	.button.primary {
		background: #18181b;
		border-color: #18181b;
		color: white;
		padding: 0.6rem 1rem;
		text-align: center;
	}
	.quiet {
		border-color: transparent !important;
		color: #52525b;
	}
	.gate {
		max-width: 26rem;
		margin: 12vh auto 0;
		padding: 0 1rem;
		text-align: center;
	}
	.gate h1 {
		font-size: 1.3rem;
	}
	.gate p {
		color: #52525b;
	}
	.providers {
		display: grid;
		gap: 0.5rem;
		margin-top: 1.5rem;
	}
	.viewonly {
		position: absolute;
		top: 12px;
		left: 50%;
		transform: translateX(-50%);
		margin: 0;
		padding: 0.35rem 0.8rem;
		border-radius: 999px;
		background: #e0e7ff;
		color: #3730a3;
		font-size: 0.85rem;
		font-weight: 600;
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
		.chat-pane {
			position: absolute;
			inset: 0;
			z-index: 20;
			width: auto;
		}
	}
</style>
