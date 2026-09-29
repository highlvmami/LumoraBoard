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

export type Invite = { url: string; code: string };

export async function createInvite(board: string, role: 'editor' | 'viewer', fetcher: typeof fetch = fetch): Promise<Invite> {
	const res = await fetcher(`/api/boards/${encodeURIComponent(board)}/invites`, {
		method: 'POST',
		credentials: 'same-origin',
		headers: { 'Content-Type': 'application/json' },
		body: JSON.stringify({ role })
	});
	if (!res.ok) throw new Error((await res.text()).trim() || `invite: ${res.status}`);
	const body = await res.json();
	return { url: body.url, code: body.code ?? body.token };
}

/** Signs in with just a name, on servers that allow it (dev login). */
export async function devLogin(name: string, fetcher: typeof fetch = fetch): Promise<void> {
	const res = await fetcher('/auth/dev/login', {
		method: 'POST',
		credentials: 'same-origin',
		body: new URLSearchParams({ name, next: '/' })
	});
	if (!res.ok) throw new Error((await res.text()).trim() || `sign in: ${res.status}`);
}

/**
 * Reads what someone pasted into "join a room": an invite link, a link
 * to a board, a bare invite code, or a board name when the server has no
 * sign-in. Returns null for something unusable.
 */
export function parseJoinInput(raw: string): { invite?: string; room?: string } | null {
	const text = raw.trim();
	if (!text) return null;
	if (/^https?:\/\//i.test(text) || text.startsWith('/?') || text.startsWith('?')) {
		let url: URL;
		try {
			url = new URL(text, 'http://x');
		} catch {
			return null;
		}
		const invite = url.searchParams.get('invite') ?? undefined;
		const room = url.searchParams.get('room') ?? undefined;
		return invite || room ? { invite, room } : null;
	}
	if (/^[A-Za-z0-9]{4}-?[A-Za-z0-9]{4}$/.test(text) || /^[0-9a-f]{64}$/i.test(text)) return { invite: text };
	if (/^[A-Za-z0-9_-]{1,64}$/.test(text)) return { room: text };
	return null;
}

export async function acceptInvite(token: string, fetcher: typeof fetch = fetch): Promise<{ board: string; role: Role }> {
	const res = await fetcher(`/api/invites/${encodeURIComponent(token)}/accept`, {
		method: 'POST',
		credentials: 'same-origin'
	});
	if (!res.ok) throw new Error((await res.text()).trim() || `invite: ${res.status}`);
	return res.json();
}
