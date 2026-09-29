<script lang="ts">
	import { onDestroy, onMount } from 'svelte';
	import { checkHealth, type BackendStatus } from '$lib/health';
	import { BoardStore, sorted, type BoardObject, type Op } from '$lib/board';
	import { connectRoom, type ConnectionState, type Envelope, type RoomConnection } from '$lib/ws';

	let health = $state<BackendStatus>('checking');
	let room = $state('demo');
	let socket = $state<ConnectionState>('closed');
	let detail = $state('');
	let notice = $state('');
	let log = $state<Envelope[]>([]);

	// The store is a plain object; `tick` bumps whenever it changes so the
	// derived values below re-read it.
	let tick = $state(0);
	let store = newStore();
	let conn: RoomConnection | null = null;

	function newStore() {
		return new BoardStore((e) => {
			if (e.type === 'rejected') notice = `rejected ${e.clientOpId}: ${e.reason}`;
			tick++;
		});
	}

	let objects = $derived.by((): BoardObject[] => {
		void tick;
		return sorted(store.view);
	});
	let clientId = $derived.by(() => {
		void tick;
		return store.clientId;
	});
	let seq = $derived.by(() => {
		void tick;
		return store.seq;
	});
	let others = $derived.by(() => {
		void tick;
		return store.members.size;
	});
	let pendingCount = $derived.by(() => {
		void tick;
		return store.pending.length;
	});
	let selected = $state('');

	onMount(async () => {
		health = await checkHealth();
	});
	onDestroy(() => conn?.close());

	function connect() {
		conn?.close();
		store = newStore();
		log = [];
		notice = '';
		selected = '';
		tick++;
		conn = connectRoom(
			room,
			{
				onMessage: (env) => {
					if (store.receive(env)) tick++;
					log = [...log.slice(-49), env];
				},
				onState: (s, d) => {
					socket = s;
					detail = d ?? '';
				}
			},
			{ since: () => store.seq }
		);
	}

	function disconnect() {
		conn?.close();
		conn = null;
	}

	function send(op: Op) {
		if (!conn) return;
		try {
			const env = conn.send(op);
			store.local(env.clientOpId!, op);
			tick++;
		} catch (e) {
			notice = String(e);
		}
	}

	let counter = 0;
	function addRect() {
		const id = `${store.clientId.slice(0, 4)}-${Date.now().toString(36)}-${counter++}`;
		const n = objects.length;
		send({
			kind: 'add',
			id,
			object: { id, kind: 'rect', x: 20 + n * 30, y: 20 + n * 20, w: 80, h: 50, color: '#3b82f6', z: n }
		});
		selected = id;
	}

	function nudge(dx: number, dy: number) {
		const o = store.view.get(selected);
		if (!o) return;
		send({ kind: 'update', id: o.id, patch: { x: o.x + dx, y: o.y + dy } });
	}

	function remove() {
		if (!selected) return;
		send({ kind: 'delete', id: selected });
		selected = '';
	}
</script>

<svelte:head>
	<title>LumoraBoard</title>
</svelte:head>

<main>
	<h1>LumoraBoard</h1>
	<p>Backend: <strong data-status={health}>{health}</strong></p>

	<section>
		<h2>Room</h2>
		<form
			onsubmit={(e) => {
				e.preventDefault();
				connect();
			}}
		>
			<input bind:value={room} pattern={"[A-Za-z0-9_\\-]{1,64}"} required aria-label="room name" />
			<button type="submit">Connect</button>
			<button type="button" onclick={disconnect} disabled={socket === 'closed'}>Disconnect</button>
		</form>
		<p>
			Socket: <strong data-status={socket}>{socket}</strong>
			{#if detail}<span class="muted">({detail})</span>{/if}
			{#if clientId}
				<span class="muted">· you are {clientId} · seq {seq} · {others} other{others === 1 ? '' : 's'}</span>
			{/if}
		</p>
	</section>

	<section>
		<h2>
			Objects <span class="muted">({objects.length}{#if pendingCount}, {pendingCount} pending{/if})</span>
		</h2>
		<div class="toolbar">
			<button type="button" onclick={addRect} disabled={socket !== 'open'}>Add rect</button>
			<button type="button" onclick={() => nudge(10, 0)} disabled={!selected || socket !== 'open'}>Move right</button>
			<button type="button" onclick={() => nudge(0, 10)} disabled={!selected || socket !== 'open'}>Move down</button>
			<button type="button" onclick={remove} disabled={!selected || socket !== 'open'}>Delete</button>
		</div>
		{#if notice}<p class="notice">{notice}</p>{/if}
		<ul class="objects">
			{#each objects as o (o.id)}
				<li>
					<label>
						<input type="radio" name="selected" value={o.id} bind:group={selected} />
						<code data-object={o.id}>{o.kind} {o.id} at {o.x},{o.y} v{o.version}{#if o.createdBy === clientId} (yours){/if}</code>
					</label>
				</li>
			{/each}
		</ul>
	</section>

	<section>
		<h2>Messages</h2>
		<ol reversed>
			{#each [...log].reverse() as env, i (log.length - i)}
				<li>
					<code>
						{#if env.seq}#{env.seq}{/if}
						{env.type}
						{#if env.from}from {env.from === clientId ? 'you' : env.from}{/if}
						{#if env.payload !== undefined}{JSON.stringify(env.payload).slice(0, 120)}{/if}
					</code>
				</li>
			{/each}
		</ol>
	</section>
</main>

<style>
	main {
		font-family: system-ui, sans-serif;
		max-width: 44rem;
		margin: 3rem auto;
		padding: 0 1rem;
	}
	section {
		margin-top: 1.5rem;
	}
	form,
	.toolbar {
		display: flex;
		gap: 0.5rem;
		flex-wrap: wrap;
	}
	input:not([type='radio']) {
		flex: 1;
		padding: 0.4rem;
	}
	.muted {
		color: #6b7280;
		font-weight: normal;
		font-size: 0.9rem;
	}
	.notice {
		color: #b45309;
	}
	.objects {
		list-style: none;
		padding: 0;
	}
	ol {
		font-size: 0.85rem;
		max-height: 16rem;
		overflow: auto;
	}
	[data-status='ok'],
	[data-status='open'] {
		color: #15803d;
	}
	[data-status='reconnecting'] {
		color: #b45309;
	}
	[data-status='down'],
	[data-status='closed'] {
		color: #b91c1c;
	}
</style>
