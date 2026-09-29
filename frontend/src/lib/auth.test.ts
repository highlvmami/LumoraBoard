import { describe, expect, it, vi } from 'vitest';
import { acceptInvite, checkAccess, createInvite, devLogin, fetchAuthStatus, loginUrl, parseJoinInput } from './auth';

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

	it('reads pasted links and codes', () => {
		expect(parseJoinInput('https://lumora.example/?room=b1&invite=K7QM-2XRA')).toEqual({ invite: 'K7QM-2XRA', room: 'b1' });
		expect(parseJoinInput('  k7qm2xra ')).toEqual({ invite: 'k7qm2xra' });
		expect(parseJoinInput('K7QM-2XRA')).toEqual({ invite: 'K7QM-2XRA' });
		expect(parseJoinInput('https://lumora.example/?room=b1')).toEqual({ invite: undefined, room: 'b1' });
		expect(parseJoinInput('my-board')).toEqual({ room: 'my-board' });
		expect(parseJoinInput('https://lumora.example/')).toBeNull();
		expect(parseJoinInput('not a code!')).toBeNull();
		expect(parseJoinInput('')).toBeNull();
	});

	it('signs in with a name', async () => {
		const f = vi.fn(async () => new Response('', { status: 200 }));
		await devLogin('Ayşe', f as unknown as typeof fetch);
		const init = (f.mock.calls[0] as unknown as [string, RequestInit])[1];
		expect(String(init.body)).toBe('name=Ay%C5%9Fe&next=%2F');
		const bad = vi.fn(async () => new Response('name must be 1 to 32 characters\n', { status: 400 }));
		await expect(devLogin('', bad as unknown as typeof fetch)).rejects.toThrow('1 to 32');
	});

	it('checks board access before the socket', async () => {
		const status = (code: number) => vi.fn(async () => new Response('', { status: code })) as unknown as typeof fetch;
		expect(await checkAccess('b', status(200))).toBe('ok');
		expect(await checkAccess('b', status(401))).toBe('signin');
		expect(await checkAccess('b', status(403))).toBe('forbidden');
		expect(await checkAccess('b', status(500))).toBe('ok');
		const down = vi.fn(async () => {
			throw new TypeError('offline');
		}) as unknown as typeof fetch;
		expect(await checkAccess('b', down)).toBe('ok');
	});

	it('creates and accepts invites, surfacing server messages', async () => {
		const ok = vi.fn(async () => json({ url: 'http://x/?room=b&invite=AB12-CD34', code: 'AB12-CD34' }, 201));
		expect(await createInvite('b', 'viewer', ok as unknown as typeof fetch)).toEqual({
			url: 'http://x/?room=b&invite=AB12-CD34',
			code: 'AB12-CD34'
		});
		expect(JSON.parse((ok.mock.calls[0] as unknown as [string, RequestInit])[1].body as string)).toEqual({ role: 'viewer' });

		const gone = vi.fn(async () => new Response('this invite link is invalid or has expired\n', { status: 404 }));
		await expect(acceptInvite('t', gone as unknown as typeof fetch)).rejects.toThrow('invalid or has expired');
	});
});
