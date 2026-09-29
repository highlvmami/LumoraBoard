import { describe, expect, it, vi } from 'vitest';
import { acceptInvite, createInvite, fetchAuthStatus, loginUrl } from './auth';

const json = (body: unknown, status = 200) =>
	new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });

describe('auth client', () => {
	it('builds provider login URLs that come back to the page', () => {
		expect(loginUrl('github', '/?room=a&invite=t')).toBe('/auth/github/login?next=%2F%3Froom%3Da%26invite%3Dt');
	});

	it('reads the status', async () => {
		const f = vi.fn(async () => json({ enabled: true, guests: false, providers: [{ id: 'github', name: 'GitHub' }] }));
		const s = await fetchAuthStatus(f as unknown as typeof fetch);
		expect(s.providers[0].id).toBe('github');
		expect(s.user).toBeUndefined();
	});

	it('creates and accepts invites, surfacing server messages', async () => {
		const ok = vi.fn(async () => json({ url: 'http://x/?room=b&invite=t' }, 201));
		expect(await createInvite('b', 'viewer', ok as unknown as typeof fetch)).toContain('invite=t');
		expect(JSON.parse((ok.mock.calls[0] as unknown as [string, RequestInit])[1].body as string)).toEqual({ role: 'viewer' });

		const gone = vi.fn(async () => new Response('this invite link is invalid or has expired\n', { status: 404 }));
		await expect(acceptInvite('t', gone as unknown as typeof fetch)).rejects.toThrow('invalid or has expired');
	});
});
