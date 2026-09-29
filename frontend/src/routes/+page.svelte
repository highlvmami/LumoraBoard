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
		<strong class="logo">LumoraBoard</strong>
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
					<button type="button" aria-expanded={shareOpen} onclick={() => (shareOpen = !shareOpen)}>Invite</button>
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
			<p class="status">
				<span class="dot" data-status={socket}></span>
				<strong data-status={socket}>{socket}</strong>
				{#if detail && socket !== 'open'}<span class="muted">({detail})</span>{/if}
				<span class="muted">· backend <strong data-status={health}>{health}</strong></span>
			</p>
		{:else}
			<p class="status"><span class="muted">backend <strong data-status={health}>{health}</strong></span></p>
		{/if}
	</header>

	<main>
		<div class="stage">
			{#if screen === 'loading'}
				<p class="gate muted">Loading…</p>
			{:else if screen === 'name'}
				<section class="gate">
					<h1>Welcome to LumoraBoard</h1>
					{#if hasDevLogin || !auth?.enabled}
						<p>What should others call you?</p>
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
				<section class="gate">
					<h1>Hi {me}</h1>
					{#if menuStep === 'choose'}
						<p>Start a board of your own, or join one you were invited to.</p>
						<div class="providers">
							<button type="button" class="button primary" onclick={createRoom}>Create a room</button>
							<button type="button" class="button" onclick={() => ((menuStep = 'join'), (formError = ''))}>Join a room</button>
						</div>
					{:else}
						<p>Paste the invite link you got, or type its code.</p>
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
				<section class="gate">
					<h1>No access to this room</h1>
					<p>Ask the room's owner for an invite link or code.</p>
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
	.menu.start {
		left: 0;
		right: auto;
	}
	.menu button {
		border: 0;
		text-align: left;
	}
	.menu button:hover {
		background: #f4f4f5;
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
		padding: 0.45rem 0.7rem;
		border: 1px solid #e4e4e7;
		border-radius: 8px;
		background: white;
		box-shadow: 0 4px 16px rgba(0, 0, 0, 0.08);
		font-size: 0.85rem;
	}
	.job progress {
		width: 6rem;
	}
	.job button {
		border: 0;
		background: none;
		color: #b91c1c;
		font: inherit;
		cursor: pointer;
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
	.stack {
		display: grid;
		gap: 0.5rem;
		margin-top: 1.5rem;
	}
	.stack input {
		width: 100%;
		box-sizing: border-box;
		padding: 0.6rem 0.75rem;
		font-size: 1rem;
	}
	.error {
		color: #b91c1c !important;
	}
	.room-name {
		font-family: ui-monospace, monospace;
		font-size: 0.85rem;
		color: #52525b;
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
		font-family: ui-monospace, monospace;
		font-weight: 600;
		letter-spacing: 0.05em;
		padding-right: 0.5rem;
	}
	.hint {
		margin: 0.25rem 0.5rem 0.25rem;
		font-size: 0.8rem;
		color: #71717a;
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
