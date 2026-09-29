import { describe, expect, it } from 'vitest';
import { checkHealth } from './health';

const respond = (status: number, body: unknown) => async () =>
	new Response(JSON.stringify(body), { status });

describe('checkHealth', () => {
	it('reports ok when the backend says ok', async () => {
		expect(await checkHealth(respond(200, { status: 'ok' }))).toBe('ok');
	});

	it('reports down on a non-2xx response', async () => {
		expect(await checkHealth(respond(503, { status: 'ok' }))).toBe('down');
	});

	it('reports down when the request fails', async () => {
		const failing = async () => {
			throw new TypeError('network');
		};
		expect(await checkHealth(failing)).toBe('down');
	});
});
