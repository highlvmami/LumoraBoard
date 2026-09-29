/** Sign-in state and the few REST calls around it. */

import type { Role } from './board';

export type User = { id: string; name: string; avatar?: string };
export type Provider = { id: string; name: string };
export type AuthStatus = {
	/** False when the server has no sign-in configured: everyone can edit. */
	enabled: boolean;
	/** Whether people who are not signed in may watch boards read-only. */
	guests: boolean;
	providers: Provider[];
	user?: User;
};

export async function fetchAuthStatus(fetcher: typeof fetch = fetch): Promise<AuthStatus> {
	const res = await fetcher('/api/auth', { credentials: 'same-origin' });
	if (!res.ok) throw new Error(`auth status: ${res.status}`);
	return res.json();
}

/**
 * Asks whether this browser may open a board before a socket is opened:
 * a refused socket can only say why with a close code, which some
 * proxies lose. Anything unexpected answers 'ok' and leaves it to the
 * socket.
 */
export async function checkAccess(
	board: string,
	fetcher: typeof fetch = fetch
): Promise<'ok' | 'signin' | 'forbidden'> {
	try {
		const res = await fetcher(`/api/boards/${encodeURIComponent(board)}/access`, { credentials: 'same-origin' });
		if (res.status === 401) return 'signin';
		if (res.status === 403) return 'forbidden';
	} catch {
		// Offline or an old server: the socket will tell.
	}
	return 'ok';
}

/** Where a provider button sends the browser; it comes back to `next`. */
export function loginUrl(provider: string, next: string): string {
	return `/auth/${encodeURIComponent(provider)}/login?next=${encodeURIComponent(next)}`;
}

export async function logout(fetcher: typeof fetch = fetch): Promise<void> {
	await fetcher('/auth/logout', { method: 'POST', credentials: 'same-origin' });
}

export async function createInvite(board: string, role: 'editor' | 'viewer', fetcher: typeof fetch = fetch): Promise<string> {
	const res = await fetcher(`/api/boards/${encodeURIComponent(board)}/invites`, {
		method: 'POST',
		credentials: 'same-origin',
		headers: { 'Content-Type': 'application/json' },
		body: JSON.stringify({ role })
	});
	if (!res.ok) throw new Error((await res.text()).trim() || `invite: ${res.status}`);
	return (await res.json()).url;
}

export async function acceptInvite(token: string, fetcher: typeof fetch = fetch): Promise<{ board: string; role: Role }> {
	const res = await fetcher(`/api/invites/${encodeURIComponent(token)}/accept`, {
		method: 'POST',
		credentials: 'same-origin'
	});
	if (!res.ok) throw new Error((await res.text()).trim() || `invite: ${res.status}`);
	return res.json();
}
