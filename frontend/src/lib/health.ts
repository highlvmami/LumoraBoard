export type BackendStatus = 'checking' | 'ok' | 'down';

/** Asks the backend whether it is up. Any failure counts as down. */
export async function checkHealth(fetchFn: typeof fetch = fetch): Promise<BackendStatus> {
	try {
		const res = await fetchFn('/healthz');
		if (!res.ok) return 'down';
		const body = await res.json();
		return body?.status === 'ok' ? 'ok' : 'down';
	} catch {
		return 'down';
	}
}
