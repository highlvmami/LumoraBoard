<script lang="ts">
	import { onDestroy, onMount } from 'svelte';
	import { checkHealth, type BackendStatus } from '$lib/health';
	import { connectRoom, type ConnectionState, type Envelope, type RoomConnection } from '$lib/ws';

	let health = $state<BackendStatus>('checking');
	let room = $state('demo');
	let socket = $state<ConnectionState>('closed');
	let detail = $state('');
	let clientId = $state('');
	let payload = $state('{"kind":"ping"}');
	let log = $state<Envelope[]>([]);
	let conn: RoomConnection | null = null;

	onMount(async () => {
		health = await checkHealth();
	});
	onDestroy(() => conn?.close());

	function connect() {
		conn?.close();
		log = [];
		clientId = '';
		conn = connectRoom(room, {
			onMessage: (env) => {
				if (env.type === 'hello') {
					clientId = (env.payload as { clientId: string }).clientId;
				}
				log = [...log.slice(-199), env];
			},
			onState: (s, d) => {
				socket = s;
				detail = d ?? '';
			}
		});
	}

	function disconnect() {
		conn?.close();
		conn = null;
	}

	function send() {
		let parsed: unknown;
		try {
			parsed = JSON.parse(payload);
		} catch {
			parsed = payload;
		}
		conn?.send(parsed);
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
		<form onsubmit={(e) => { e.preventDefault(); connect(); }}>
			<input bind:value={room} pattern={"[A-Za-z0-9_\\-]{1,64}"} required aria-label="room name" />
			<button type="submit">Connect</button>
			<button type="button" onclick={disconnect} disabled={socket === 'closed'}>Disconnect</button>
		</form>
		<p>
			Socket: <strong data-status={socket}>{socket}</strong>
			{#if detail}<span class="muted">({detail})</span>{/if}
			{#if clientId}<span class="muted">· you are {clientId}</span>{/if}
		</p>
	</section>

	<section>
		<h2>Send op</h2>
		<form onsubmit={(e) => { e.preventDefault(); send(); }}>
			<input bind:value={payload} aria-label="payload" />
			<button type="submit" disabled={socket !== 'open'}>Send</button>
		</form>
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
						{#if env.payload !== undefined}{JSON.stringify(env.payload)}{/if}
					</code>
				</li>
			{/each}
		</ol>
	</section>
</main>

<style>
	main {
		font-family: system-ui, sans-serif;
		max-width: 40rem;
		margin: 3rem auto;
		padding: 0 1rem;
	}
	section {
		margin-top: 1.5rem;
	}
	form {
		display: flex;
		gap: 0.5rem;
	}
	input {
		flex: 1;
		padding: 0.4rem;
	}
	.muted {
		color: #6b7280;
	}
	ol {
		font-size: 0.9rem;
		max-height: 24rem;
		overflow: auto;
	}
	[data-status='ok'],
	[data-status='open'] {
		color: #15803d;
	}
	[data-status='down'],
	[data-status='closed'] {
		color: #b91c1c;
	}
</style>
