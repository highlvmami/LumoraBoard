<script lang="ts">
	import { onMount } from 'svelte';
	import { checkHealth, type BackendStatus } from '$lib/health';

	let status = $state<BackendStatus>('checking');

	onMount(async () => {
		status = await checkHealth();
	});
</script>

<svelte:head>
	<title>LumoraBoard</title>
</svelte:head>

<main>
	<h1>LumoraBoard</h1>
	<p>Backend: <strong data-status={status}>{status}</strong></p>
</main>

<style>
	main {
		font-family: system-ui, sans-serif;
		max-width: 40rem;
		margin: 4rem auto;
		padding: 0 1rem;
	}
	[data-status='ok'] {
		color: #15803d;
	}
	[data-status='down'] {
		color: #b91c1c;
	}
</style>
