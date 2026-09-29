<script lang="ts">
	import { onDestroy, onMount } from 'svelte';
	import { page } from '$app/state';
	import { replaceState } from '$app/navigation';
	import { checkHealth, type BackendStatus } from '$lib/health';
	import { BoardStore, canEdit, sorted, type BoardObject, type Op, type Point, type Role } from '$lib/board';
	import { colorFor, displayName, Presence, throttle } from '$lib/presence';
	import { CLOSE_FORBIDDEN, CLOSE_UNAUTHORIZED, connectRoom, type ConnectionState, type RoomConnection } from '$lib/ws';
	import {
		acceptInvite,
		checkAccess,
		createInvite,
		devLogin,
		fetchAuthStatus,
		loginUrl,
		logout,
		parseJoinInput,
		type AuthStatus,
		type Invite
	} from '$lib/auth';
	import Whiteboard, { type RemoteCursor } from '$lib/Whiteboard.svelte';
	import Chat from '$lib/Chat.svelte';
	import { ChatStore, fetchOlder, typingLabel } from '$lib/chat';
	import {
		cancelExport,
		download,
		exportStatus,
		importBackup,
		jobFinished,
		requestExport,
		toPNG,
		toSVG,
		type ExportJob,
		type ServerFormat
	} from '$lib/export';

	const NAME_KEY = 'lumora.name';
	const ROOM_RE = /^[A-Za-z0-9_-]{1,64}$/;

	let health = $state<BackendStatus>('checking');
	let room = $state('');
	/**
	 * What the page shows: nothing yet, the name step, the join-or-create
	 * menu, or a board.
	 */
	let screen = $state<'loading' | 'name' | 'menu' | 'board'>('loading');
	let menuStep = $state<'choose' | 'join'>('choose');
	let joinText = $state('');
	let formError = $state('');
	let busy = $state(false);
	/** Invites the owner made in this visit, shown in the share menu. */
	let invites = $state<Partial<Record<'editor' | 'viewer', Invite>>>({});
	let name = $state('');
	let socket = $state<ConnectionState>('closed');
	let detail = $state('');
	let notice = $state('');
	/** Null until the server said how sign-in works. */
	let auth = $state<AuthStatus | null>(null);
	/** Why the board is not shown, if it is not. */
	let gate = $state<'' | 'forbidden'>('');
	let shareOpen = $state(false);
	let chatOpen = $state(false);
	let exportOpen = $state(false);
	/** The server export in flight or just finished, if any. */
	let job = $state<ExportJob | null>(null);
	let importInput = $state<HTMLInputElement>();
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

	// ---- export / import ---------------------------------------------

	const FORMAT_LABEL: Record<ServerFormat, string> = { png: 'high-res PNG', pdf: 'PDF', json: 'JSON backup' };

	async function exportLocal(kind: 'png' | 'svg') {
		exportOpen = false;
		try {
			if (kind === 'svg') download(new Blob([toSVG(objects)], { type: 'image/svg+xml' }), `${room}.svg`);
			else download(await toPNG(objects, 2), `${room}.png`);
		} catch (e) {
			flash(e instanceof Error ? e.message : String(e));
		}
	}

	let jobPoll: ReturnType<typeof setInterval> | undefined;

	async function exportServer(format: ServerFormat) {
		exportOpen = false;
		try {
			track(await requestExport(room, format, clientId, format === 'png' ? 4 : 1));
		} catch (e) {
			flash(e instanceof Error ? e.message : String(e));
		}
	}

	/** Takes a status from the socket, a poll or a request. */
	function track(next: ExportJob) {
		// Socket messages and REST answers can cross: nothing moves a job
		// back from a final state, and a message for an older job while a
		// newer one runs is stale.
		if (job?.id === next.id && jobFinished(job)) return;
		if (job && job.id !== next.id && !jobFinished(job)) return;
		job = next;
		clearInterval(jobPoll);
		if (!jobFinished(next)) {
			// Progress comes over the socket; polling covers a lost message
			// or a reconnect in between.
			jobPoll = setInterval(async () => {
				if (!job || jobFinished(job)) return clearInterval(jobPoll);
				try {
					track(await exportStatus(job.id));
				} catch {
					// Try again on the next tick.
				}
			}, 2000);
			return;
		}
		if (next.state === 'done' && next.file) download(next.file, `${next.board}.${next.format}`);
		if (next.state === 'failed') flash(`Export failed: ${next.error ?? 'unknown error'}`);
		setTimeout(() => {
			if (job?.id === next.id) job = null;
		}, 4000);
	}

	async function stopExport() {
		if (!job) return;
		await cancelExport(job.id);
		track({ ...job, state: 'canceled' });
	}

	async function onImport(e: Event) {
		const input = e.currentTarget as HTMLInputElement;
		const file = input.files?.[0];
		input.value = '';
		exportOpen = false;
		if (!file) return;
		try {
			const res = await importBackup(file);
			openRoom(res.board);
			flash(`Imported ${res.objects} objects into a new board.`);
		} catch (err) {
			flash(err instanceof Error ? err.message : String(err));
		}
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
		checkHealth().then((h) => (health = h));
		try {
			auth = await fetchAuthStatus();
		} catch {
			// An old or unreachable backend: behave as before sign-in existed.
			auth = { enabled: false, guests: false, providers: [] };
		}
		await route();
	});

	/** Whether the visitor has told us who they are. */
	let named = $derived(auth ? (auth.enabled ? !!auth.user : !!name.trim()) : false);
	let hasDevLogin = $derived(!!auth?.providers.some((p) => p.id === 'dev'));
	/** Sign-in buttons other than the name form. */
	let otherProviders = $derived(auth?.providers.filter((p) => p.id !== 'dev') ?? []);

	/**
	 * Picks the screen from the URL: a name first, then an invite or a
	 * board the link points at, else the join-or-create menu.
	 */
	async function route() {
		const invite = page.url.searchParams.get('invite');
		const fromUrl = page.url.searchParams.get('room') ?? '';
		if (!named) {
			// Guests may watch a board they have a link to without a name.
			if (auth?.guests && fromUrl && !invite) {
				openRoom(fromUrl);
				return;
			}
			screen = 'name';
			return;
		}
		if (invite && auth?.enabled) {
			await redeem(invite);
			return;
		}
		if (ROOM_RE.test(fromUrl)) {
			openRoom(fromUrl);
			return;
		}
		screen = 'menu';
	}

	async function submitName(e: SubmitEvent) {
		e.preventDefault();
		const n = name.trim();
		if (!n || busy) return;
		busy = true;
		formError = '';
		try {
			localStorage.setItem(NAME_KEY, n);
		} catch {
			// See onMount.
		}
		try {
			if (auth?.enabled) {
				await devLogin(n);
				auth = await fetchAuthStatus();
			}
			await route();
		} catch (err) {
			formError = err instanceof Error ? err.message : String(err);
		} finally {
			busy = false;
		}
	}

	/** Accepts an invite and opens its board with the role it grants. */
	async function redeem(code: string) {
		try {
			const res = await acceptInvite(code.trim());
			openRoom(res.board);
			flash(`You joined as ${res.role === 'viewer' ? 'a viewer' : res.role === 'owner' ? 'the owner' : 'an editor'}.`);
		} catch (err) {
			screen = 'menu';
			menuStep = 'join';
			formError = err instanceof Error ? err.message : String(err);
			setUrl('');
		}
	}

	async function submitJoin(e: SubmitEvent) {
		e.preventDefault();
		if (busy) return;
		const parsed = parseJoinInput(joinText);
		if (!parsed) {
			formError = 'Paste an invite link or type the invite code.';
			return;
		}
		formError = '';
		busy = true;
		try {
			if (parsed.invite && auth?.enabled) await redeem(parsed.invite);
			else if (parsed.room) openRoom(parsed.room);
			else formError = 'That code needs sign-in, which this server does not have.';
		} finally {
			busy = false;
		}
	}

	function createRoom() {
		openRoom(randomBoardName());
	}

	function setUrl(board: string) {
		const url = new URL(page.url);
		url.search = board ? `?room=${encodeURIComponent(board)}` : '';
		replaceState(url, page.state);
	}

	function openRoom(board: string) {
		if (!ROOM_RE.test(board)) {
			screen = 'menu';
			return;
		}
		room = board;
		invites = {};
		screen = 'board';
		setUrl(board);
		connect();
	}

	/** Back to the join-or-create menu. */
	function home() {
		conn?.close();
		conn = null;
		socket = 'closed';
		room = '';
		gate = '';
		joinText = '';
		formError = '';
		menuStep = 'choose';
		shareOpen = chatOpen = exportOpen = false;
		setUrl('');
		screen = named ? 'menu' : 'name';
	}

	onDestroy(() => {
		conn?.close();
		sendCursor.cancel();
		sendTyping.cancel();
		clearInterval(typingTimer);
		clearInterval(jobPoll);
	});

	async function connect() {
		conn?.close();
		conn = null;
		gate = '';
		// Asked over HTTP first: a refused socket's close code does not
		// always make it through the host's proxy.
		const target = room;
		const access = await checkAccess(target);
		if (room !== target || conn) return; // another connect() took over
		if (access === 'signin') {
			screen = 'name';
			return;
		}
		if (access === 'forbidden') {
			gate = 'forbidden';
			return;
		}
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
					if (env.type === 'export.progress') {
						track(env.payload as ExportJob);
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
					if (code === CLOSE_UNAUTHORIZED) screen = 'name';
					else if (code === CLOSE_FORBIDDEN) gate = 'forbidden';
				}
			},
			{ since: () => store.seq, name: name.trim() }
		);
	}

	function randomBoardName(): string {
		const bytes = crypto.getRandomValues(new Uint8Array(4));
		return 'board-' + Array.from(bytes, (b) => b.toString(16).padStart(2, '0')).join('');
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

	/** Makes an invite for the role, once per visit, and copies its link. */
	async function invite(r: 'editor' | 'viewer') {
		try {
			const inv = invites[r] ?? (await createInvite(room, r));
			invites[r] = inv;
			copy(inv.url, r === 'editor' ? 'Edit link' : 'View link');
		} catch (e) {
			flash(e instanceof Error ? e.message : String(e));
		}
	}

	async function signOut() {
		if (auth?.enabled) await logout();
		try {
			localStorage.removeItem(NAME_KEY);
		} catch {
			// See onMount.
		}
		location.href = '/';
	}

	// Where sign-in comes back to: this page, on the current board.
	let here = $derived.by(() => {
		const url = new URL(page.url);
		if (room) url.searchParams.set('room', room);
		return url.pathname + url.search;
	});
</script>

<svelte:head>
	<title>{room ? `${room} · LumoraBoard` : 'LumoraBoard'}</title>
</svelte:head>

<div class="app">
	<header>
		<strong class="logo">
			<svg class="mark" viewBox="0 0 32 32" width="26" height="26" aria-hidden="true">
				<defs
					><linearGradient id="lb-mark" x1="0" y1="0" x2="1" y2="1"
						><stop offset="0" stop-color="#6366f1" /><stop offset="1" stop-color="#a855f7" /></linearGradient
					></defs
				>
				<rect width="32" height="32" rx="8" fill="url(#lb-mark)" />
				<path d="M8.5 21.5c2.5-6 5.5-9 7.5-6.5s4 3 7.5-5" fill="none" stroke="#fff" stroke-width="2.6" stroke-linecap="round" stroke-linejoin="round" />
			</svg>
			<span class="wordmark">LumoraBoard</span>
		</strong>
		{#if screen === 'board'}
			<button type="button" class="quiet" onclick={home} title="Back to the menu">← Menu</button>
			<span class="room-name" title="Board">{room}</span>
		{/if}
		<!-- Room controls only for people who can open this board. -->
		{#if screen === 'board' && !gate}
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
				<button type="button" aria-expanded={exportOpen} onclick={() => (exportOpen = !exportOpen)} disabled={!!gate}>Export</button>
				{#if exportOpen}
					<div class="menu start" role="menu">
						<button type="button" role="menuitem" onclick={() => exportLocal('png')}>PNG image</button>
						<button type="button" role="menuitem" onclick={() => exportLocal('svg')}>SVG vector</button>
						<hr />
						<button type="button" role="menuitem" onclick={() => exportServer('png')} disabled={!!job && !jobFinished(job)}
							>High-res PNG (4×)</button
						>
						<button type="button" role="menuitem" onclick={() => exportServer('pdf')} disabled={!!job && !jobFinished(job)}>PDF</button>
						<button type="button" role="menuitem" onclick={() => exportServer('json')} disabled={!!job && !jobFinished(job)}
							>JSON backup</button
						>
						{#if !auth?.enabled || auth.user}
							<hr />
							<button type="button" role="menuitem" onclick={() => importInput?.click()}>Import backup…</button>
						{/if}
					</div>
				{/if}
				<input bind:this={importInput} type="file" accept="application/json,.json" hidden onchange={onImport} />
			</div>
			<div class="share-wrap">
				{#if auth?.enabled && role === 'owner'}
					<button type="button" class="accent" aria-expanded={shareOpen} onclick={() => (shareOpen = !shareOpen)}>Invite</button>
					{#if shareOpen}
						<div class="menu invites" role="menu">
							{#each [['editor', 'Can edit'], ['viewer', 'View only']] as const as [r, label] (r)}
								<div class="invite-row">
									<button type="button" role="menuitem" onclick={() => invite(r)}>Copy {label.toLowerCase()} link</button>
									{#if invites[r]}<span class="code" title="Invite code">{invites[r]!.code}</span>{/if}
								</div>
							{/each}
							<p class="hint">People open the link, or type the code under “Join a room”.</p>
						</div>
					{/if}
				{:else if !auth?.enabled}
					<button type="button" onclick={() => copy(page.url.href, 'Link')}>Share</button>
				{/if}
			</div>
		{/if}
		{#if named}
			<button type="button" class="quiet" onclick={signOut} title="Signed in as {me}">Sign out</button>
		{:else if screen === 'board' && auth?.enabled && auth.guests}
			<button type="button" onclick={() => (screen = 'name')}>Sign in to edit</button>
		{/if}
		{#if screen === 'board'}
			<p class="status pill" data-status={socket}>
				<span class="dot"></span>
				<strong data-status={socket}>{socket}</strong>
				{#if detail && socket !== 'open'}<span class="muted">({detail})</span>{/if}
				<span class="muted">· backend <strong data-status={health}>{health}</strong></span>
			</p>
		{:else}
			<p class="status"><span class="muted">backend <strong data-status={health}>{health}</strong></span></p>
		{/if}
	</header>

	<main>
		<div class="stage" class:landing={screen !== 'board' || !!gate}>
			{#if screen === 'loading'}
				<p class="gate muted">Loading…</p>
			{:else if screen === 'name'}
				<section class="gate card">
					<div class="hero-mark" aria-hidden="true">
						<svg viewBox="0 0 24 24" width="28" height="28"
							><path d="M4 17c2.5-6 5.5-9 7.5-6.5s4 3 8.5-5" fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round" /></svg
						>
					</div>
					<h1>Welcome to LumoraBoard</h1>
					<p class="lead">Draw together in real time, with live cursors and private rooms.</p>
					{#if hasDevLogin || !auth?.enabled}
						<p class="label">What should others call you?</p>
						<form class="stack" onsubmit={submitName}>
							<!-- svelte-ignore a11y_autofocus -->
							<input bind:value={name} maxlength="32" required autofocus aria-label="Your name" placeholder="Your name" />
							<button type="submit" class="button primary" disabled={busy || !name.trim()}>Continue</button>
						</form>
					{/if}
					{#if otherProviders.length}
						<div class="providers">
							{#each otherProviders as p (p.id)}
								<a class="button" href={loginUrl(p.id, here)}>Continue with {p.name}</a>
							{/each}
						</div>
					{/if}
					{#if formError}<p class="error" role="alert">{formError}</p>{/if}
				</section>
			{:else if screen === 'menu'}
				<section class="gate card">
					<h1>Hi {me}</h1>
					{#if menuStep === 'choose'}
						<p class="lead">Start a board of your own, or join one you were invited to.</p>
						<div class="tiles">
							<button type="button" class="tile" onclick={createRoom}>
								<span class="tile-icon create" aria-hidden="true">
									<svg viewBox="0 0 24 24" width="22" height="22"><path d="M12 5v14M5 12h14" fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round" /></svg>
								</span>
								<span class="tile-text">
									<strong>Create a room</strong>
									<small>A new board you own. Invite people to edit or watch.</small>
								</span>
							</button>
							<button type="button" class="tile" onclick={() => ((menuStep = 'join'), (formError = ''))}>
								<span class="tile-icon join" aria-hidden="true">
									<svg viewBox="0 0 24 24" width="22" height="22"
										><path
											d="M10 14a4 4 0 0 0 5.7 0l3-3a4 4 0 0 0-5.7-5.7l-1 1M14 10a4 4 0 0 0-5.7 0l-3 3a4 4 0 0 0 5.7 5.7l1-1"
											fill="none"
											stroke="currentColor"
											stroke-width="2"
											stroke-linecap="round"
											stroke-linejoin="round"
										/></svg
									>
								</span>
								<span class="tile-text">
									<strong>Join a room</strong>
									<small>Paste an invite link or type its code.</small>
								</span>
							</button>
						</div>
					{:else}
						<p class="lead">Paste the invite link you got, or type its code.</p>
						<form class="stack" onsubmit={submitJoin}>
							<!-- svelte-ignore a11y_autofocus -->
							<input bind:value={joinText} required autofocus aria-label="Invite link or code" placeholder="Link or code, e.g. K7QM-2XRA" />
							<button type="submit" class="button primary" disabled={busy || !joinText.trim()}>Join</button>
							<button type="button" class="quiet" onclick={() => ((menuStep = 'choose'), (formError = ''))}>Back</button>
						</form>
					{/if}
					{#if formError}<p class="error" role="alert">{formError}</p>{/if}
				</section>
			{:else if gate}
				<section class="gate card">
					<div class="hero-mark warn" aria-hidden="true">
						<svg viewBox="0 0 24 24" width="26" height="26"
							><path d="M7 11V8a5 5 0 0 1 10 0v3M6 11h12v9H6z" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" /></svg
						>
					</div>
					<h1>No access to this room</h1>
					<p class="lead">Ask the room's owner for an invite link or code.</p>
					<div class="providers">
						<button type="button" class="button primary" onclick={home}>Back to the menu</button>
					</div>
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
			{#if job}
				<div class="job" role="status" data-job-state={job.state}>
					{#if job.state === 'done'}
						<span>{FORMAT_LABEL[job.format]} ready.</span>
						<a href={job.file} download="{job.board}.{job.format}">Download again</a>
					{:else if job.state === 'failed' || job.state === 'canceled'}
						<span>Export {job.state}.</span>
					{:else}
						<span>{job.state === 'queued' ? 'Waiting to export' : 'Exporting'} {FORMAT_LABEL[job.format]}…</span>
						<progress max="100" value={job.progress}></progress>
						<button type="button" onclick={stopExport}>Cancel</button>
					{/if}
				</div>
			{/if}
			{#if socket === 'reconnecting'}<p class="banner">Connection lost, reconnecting…{#if detail}<span class="muted"> ({detail})</span>{/if}</p>{/if}
		</div>
		{#if chatOpen && !gate && screen === 'board'}
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
	:global(:root) {
		--brand: #6366f1;
		--brand-2: #a855f7;
		--brand-strong: #4f46e5;
		--brand-soft: #eef2ff;
		--brand-grad: linear-gradient(135deg, #6366f1, #a855f7);
		--ink: #18181b;
		--ink-2: #52525b;
		--ink-3: #71717a;
		--line: #e4e4e7;
		--surface: #ffffff;
		--canvas: #f8fafc;
		--ring: 0 0 0 3px rgba(99, 102, 241, 0.25);
	}
	:global(html, body) {
		margin: 0;
		height: 100%;
		font-family: Inter, system-ui, -apple-system, 'Segoe UI', sans-serif;
		color: var(--ink);
		background: var(--canvas);
		-webkit-font-smoothing: antialiased;
	}
	.app {
		display: flex;
		flex-direction: column;
		height: 100dvh;
	}
	header {
		position: relative;
		z-index: 5;
		display: flex;
		align-items: center;
		flex-wrap: wrap;
		gap: 0.5rem 0.75rem;
		padding: 0.55rem 1rem;
		background: rgba(255, 255, 255, 0.85);
		backdrop-filter: blur(10px);
		border-bottom: 1px solid var(--line);
		box-shadow: 0 1px 2px rgba(15, 23, 42, 0.04);
	}
	.logo {
		display: flex;
		align-items: center;
		gap: 0.5rem;
		margin-right: 0.25rem;
		font-size: 1.05rem;
		font-weight: 700;
		letter-spacing: -0.02em;
	}
	.mark {
		flex: none;
		filter: drop-shadow(0 2px 4px rgba(99, 102, 241, 0.35));
	}
	form {
		display: flex;
		gap: 0.4rem;
	}
	input {
		width: 8rem;
		padding: 0.35rem 0.5rem;
		border: 1px solid #d4d4d8;
		border-radius: 8px;
		font: inherit;
		font-size: 0.9rem;
		transition:
			border-color 0.15s,
			box-shadow 0.15s;
	}
	input:focus {
		outline: none;
		border-color: var(--brand);
		box-shadow: var(--ring);
	}
	header button {
		padding: 0.4rem 0.85rem;
		border: 1px solid var(--line);
		border-radius: 8px;
		background: var(--surface);
		color: var(--ink);
		font: inherit;
		font-size: 0.875rem;
		font-weight: 500;
		cursor: pointer;
		transition:
			background 0.15s,
			border-color 0.15s,
			box-shadow 0.15s;
	}
	header button:hover {
		background: #f4f4f5;
		border-color: #d4d4d8;
	}
	header button:focus-visible,
	.button:focus-visible,
	.tile:focus-visible {
		outline: none;
		box-shadow: var(--ring);
	}
	header button.accent {
		border-color: transparent;
		background: var(--brand-grad);
		color: white;
		box-shadow: 0 2px 8px rgba(99, 102, 241, 0.35);
	}
	header button.accent:hover {
		filter: brightness(1.06);
	}
	.people {
		display: flex;
		margin-left: auto;
		padding-left: 6px;
	}
	.avatar {
		display: grid;
		place-items: center;
		width: 30px;
		height: 30px;
		margin-left: -6px;
		border: 2px solid white;
		border-radius: 50%;
		background: var(--c);
		color: white;
		font-size: 0.75rem;
		font-weight: 600;
		box-shadow: 0 1px 3px rgba(15, 23, 42, 0.2);
	}
	.status {
		display: flex;
		align-items: center;
		gap: 0.35rem;
		margin: 0;
		font-size: 0.8rem;
	}
	.status.pill {
		padding: 0.25rem 0.65rem;
		border-radius: 999px;
		background: #f4f4f5;
	}
	.status.pill[data-status='open'] {
		background: #ecfdf5;
	}
	.status.pill[data-status='connecting'],
	.status.pill[data-status='reconnecting'] {
		background: #fffbeb;
	}
	.status.pill[data-status='closed'] {
		background: #fef2f2;
	}
	.status .dot {
		width: 8px;
		height: 8px;
		border-radius: 50%;
		background: currentColor;
	}
	.status.pill[data-status='open'] .dot {
		box-shadow: 0 0 0 3px rgba(22, 163, 74, 0.18);
	}
	.status strong {
		font-weight: 600;
	}
	.muted {
		color: var(--ink-3);
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
	.stage.landing {
		overflow-y: auto;
		background:
			radial-gradient(circle at 15% 10%, rgba(99, 102, 241, 0.16), transparent 45%),
			radial-gradient(circle at 85% 90%, rgba(168, 85, 247, 0.14), transparent 45%),
			radial-gradient(circle, rgba(15, 23, 42, 0.07) 1px, transparent 1.5px) 0 0 / 22px 22px,
			var(--canvas);
	}
	.chat-pane {
		flex: none;
		width: 320px;
	}
	.chat-toggle {
		position: relative;
	}
	.chat-toggle[aria-pressed='true'] {
		background: var(--brand-soft);
		border-color: #c7d2fe;
		color: var(--brand-strong);
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
		padding: 0.5rem 0.9rem;
		border-radius: 10px;
		font-size: 0.85rem;
		box-shadow: 0 8px 24px rgba(15, 23, 42, 0.15);
	}
	.notice {
		bottom: 16px;
		background: var(--ink);
		color: white;
	}
	.banner {
		top: 68px;
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
		top: calc(100% + 6px);
		z-index: 10;
		display: grid;
		min-width: 12rem;
		padding: 6px;
		background: white;
		border: 1px solid var(--line);
		border-radius: 12px;
		box-shadow: 0 12px 32px rgba(15, 23, 42, 0.14);
	}
	.menu.start {
		left: 0;
		right: auto;
	}
	.menu button {
		border: 0;
		border-radius: 8px;
		text-align: left;
		box-shadow: none;
	}
	.menu button:hover {
		background: var(--brand-soft);
		color: var(--brand-strong);
	}
	.menu button:disabled {
		color: #a1a1aa;
		background: none;
	}
	.menu hr {
		width: 100%;
		margin: 4px 0;
		border: 0;
		border-top: 1px solid #f4f4f5;
	}
	.job {
		position: absolute;
		left: 12px;
		bottom: 12px;
		display: flex;
		align-items: center;
		gap: 0.5rem;
		padding: 0.5rem 0.8rem;
		border: 1px solid var(--line);
		border-radius: 12px;
		background: white;
		box-shadow: 0 8px 24px rgba(15, 23, 42, 0.1);
		font-size: 0.85rem;
	}
	.job progress {
		width: 6rem;
		accent-color: var(--brand);
	}
	.job button {
		border: 0;
		background: none;
		color: #b91c1c;
		font: inherit;
		cursor: pointer;
	}
	.job a {
		color: var(--brand-strong);
	}
	.button {
		padding: 0.5rem 0.9rem;
		border: 1px solid #d4d4d8;
		border-radius: 10px;
		background: white;
		color: inherit;
		font: inherit;
		font-size: 0.9rem;
		font-weight: 500;
		text-decoration: none;
		cursor: pointer;
		transition:
			transform 0.15s,
			box-shadow 0.15s,
			filter 0.15s;
	}
	.button.primary {
		padding: 0.7rem 1rem;
		border-color: transparent;
		background: var(--brand-grad);
		color: white;
		font-weight: 600;
		text-align: center;
		box-shadow: 0 4px 14px rgba(99, 102, 241, 0.35);
	}
	.button.primary:hover:not(:disabled) {
		filter: brightness(1.06);
		transform: translateY(-1px);
		box-shadow: 0 6px 18px rgba(99, 102, 241, 0.42);
	}
	.button.primary:disabled {
		opacity: 0.55;
		box-shadow: none;
		cursor: default;
	}
	.quiet {
		border-color: transparent !important;
		background: transparent !important;
		color: var(--ink-2);
	}
	.stage .quiet {
		font: inherit;
		font-size: 0.9rem;
		cursor: pointer;
	}
	.quiet:hover {
		color: var(--ink);
		background: rgba(15, 23, 42, 0.05) !important;
	}
	.gate {
		max-width: 26rem;
		margin: 12vh auto 0;
		padding: 0 1rem;
		text-align: center;
	}
	.gate.card {
		box-sizing: border-box;
		width: calc(100% - 32px);
		max-width: 28rem;
		margin: max(6vh, 24px) auto 24px;
		padding: 2.25rem 2rem 2rem;
		background: rgba(255, 255, 255, 0.92);
		border: 1px solid rgba(228, 228, 231, 0.9);
		border-radius: 20px;
		box-shadow:
			0 1px 2px rgba(15, 23, 42, 0.04),
			0 20px 50px rgba(15, 23, 42, 0.1);
		animation: rise 0.35s ease-out;
	}
	@keyframes rise {
		from {
			opacity: 0;
			transform: translateY(8px);
		}
	}
	.hero-mark {
		display: grid;
		place-items: center;
		width: 56px;
		height: 56px;
		margin: 0 auto 1rem;
		border-radius: 16px;
		background: var(--brand-grad);
		color: white;
		box-shadow: 0 8px 20px rgba(99, 102, 241, 0.35);
	}
	.hero-mark.warn {
		background: linear-gradient(135deg, #f59e0b, #f43f5e);
		box-shadow: 0 8px 20px rgba(244, 63, 94, 0.3);
	}
	.gate h1 {
		margin: 0;
		font-size: 1.5rem;
		font-weight: 700;
		letter-spacing: -0.02em;
	}
	.gate p {
		color: var(--ink-2);
	}
	.gate .lead {
		margin: 0.5rem 0 0;
		line-height: 1.5;
	}
	.gate .label {
		margin: 1.75rem 0 0;
		font-size: 0.9rem;
		font-weight: 500;
		color: var(--ink);
	}
	.stack {
		display: grid;
		gap: 0.6rem;
		margin-top: 1.25rem;
	}
	.label + .stack {
		margin-top: 0.6rem;
	}
	.stack input {
		width: 100%;
		box-sizing: border-box;
		padding: 0.75rem 0.9rem;
		border-radius: 10px;
		font-size: 1rem;
		background: white;
	}
	.tiles {
		display: grid;
		gap: 0.75rem;
		margin-top: 1.5rem;
	}
	.tile {
		display: flex;
		align-items: center;
		gap: 0.9rem;
		padding: 1rem;
		border: 1px solid var(--line);
		border-radius: 14px;
		background: white;
		color: inherit;
		font: inherit;
		text-align: left;
		cursor: pointer;
		transition:
			border-color 0.15s,
			box-shadow 0.15s,
			transform 0.15s;
	}
	.tile:hover {
		border-color: #c7d2fe;
		box-shadow: 0 8px 24px rgba(99, 102, 241, 0.15);
		transform: translateY(-1px);
	}
	.tile-icon {
		flex: none;
		display: grid;
		place-items: center;
		width: 44px;
		height: 44px;
		border-radius: 12px;
	}
	.tile-icon.create {
		background: var(--brand-grad);
		color: white;
		box-shadow: 0 4px 12px rgba(99, 102, 241, 0.35);
	}
	.tile-icon.join {
		background: var(--brand-soft);
		color: var(--brand-strong);
	}
	.tile-text {
		display: grid;
		gap: 0.15rem;
	}
	.tile-text strong {
		font-size: 1rem;
		font-weight: 600;
	}
	.tile-text small {
		color: var(--ink-3);
		font-size: 0.85rem;
		line-height: 1.35;
	}
	.error {
		margin: 1rem 0 0;
		padding: 0.5rem 0.75rem;
		border-radius: 8px;
		background: #fef2f2;
		color: #b91c1c !important;
		font-size: 0.9rem;
	}
	.room-name {
		padding: 0.2rem 0.55rem;
		border-radius: 6px;
		background: #f4f4f5;
		font-family: ui-monospace, monospace;
		font-size: 0.8rem;
		color: var(--ink-2);
	}
	.invites {
		min-width: 20rem;
	}
	.invite-row {
		display: flex;
		align-items: center;
		justify-content: space-between;
		gap: 0.5rem;
	}
	.code {
		white-space: nowrap;
		padding: 0.2rem 0.5rem;
		border-radius: 6px;
		background: var(--brand-soft);
		color: var(--brand-strong);
		font-family: ui-monospace, monospace;
		font-weight: 600;
		font-size: 0.85rem;
		letter-spacing: 0.05em;
	}
	.hint {
		margin: 0.35rem 0.5rem 0.25rem;
		font-size: 0.8rem;
		color: var(--ink-3);
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
		padding: 0.4rem 0.9rem;
		border-radius: 999px;
		background: var(--brand-grad);
		color: white;
		font-size: 0.85rem;
		font-weight: 600;
		box-shadow: 0 4px 14px rgba(99, 102, 241, 0.35);
	}
	@media (max-width: 640px) {
		header {
			gap: 0.4rem 0.5rem;
			padding: 0.45rem 0.75rem;
		}
		header:has(.room-name) .wordmark {
			display: none;
		}
		header button {
			padding: 0.35rem 0.65rem;
		}
		input {
			width: 6rem;
		}
		.status .muted {
			display: none;
		}
		.gate.card {
			padding: 1.75rem 1.25rem 1.5rem;
		}
		.chat-pane {
			position: absolute;
			inset: 0;
			z-index: 20;
			width: auto;
		}
	}
</style>
