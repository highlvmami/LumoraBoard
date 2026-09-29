import { describe, expect, it, vi } from 'vitest';
import type { BoardObject } from './board';
import { exportArea, importBackup, jobFinished, requestExport, toSVG } from './export';

const obj = (o: Partial<BoardObject> & Pick<BoardObject, 'id' | 'kind'>): BoardObject => ({ x: 0, y: 0, z: 0, version: 1, ...o });

describe('exportArea', () => {
	it('is a blank page for an empty board', () => {
		expect(exportArea([])).toEqual({ x: 0, y: 0, w: 800, h: 600 });
	});
	it('covers every object plus a margin', () => {
		const a = exportArea([obj({ id: 'a', kind: 'rect', x: 10, y: 20, w: 100, h: 50 }), obj({ id: 'b', kind: 'sticky', x: 300, y: 0 })]);
		expect(a).toEqual({ x: -22, y: -32, w: 514, h: 224 });
	});
});

describe('toSVG', () => {
	it('draws every kind and escapes text', () => {
		const svg = toSVG([
			obj({ id: 'r', kind: 'rect', w: 10, h: 10, color: '#e11d48' }),
			obj({ id: 'e', kind: 'ellipse', w: 10, h: 6 }),
			obj({ id: 's', kind: 'stroke', points: [{ x: 0, y: 0 }, { x: 5, y: 5 }, { x: 10, y: 0 }] }),
			obj({ id: 'a', kind: 'arrow', points: [{ x: 0, y: 0 }, { x: 20, y: 0 }] }),
			obj({ id: 't', kind: 'text', text: '<b>"hi" & bye</b>\nline 2' }),
			obj({ id: 'n', kind: 'sticky', text: 'note', color: 'javascript:alert(1)' })
		]);
		for (const tag of ['<rect', '<ellipse', '<path', '<line', '<polygon', '<text', '<tspan']) expect(svg).toContain(tag);
		expect(svg).toContain('&lt;b&gt;&quot;hi&quot; &amp; bye&lt;/b&gt;');
		expect(svg).not.toContain('<b>');
		expect(svg).not.toContain('javascript');
		expect(svg).toContain('stroke="#e11d48"');
		expect(svg.match(/<tspan/g)).toHaveLength(3);
	});
});

describe('server jobs', () => {
	it('requests an export with the connection id', async () => {
		const fetchFn = vi.fn(async () => new Response(JSON.stringify({ id: 'j', state: 'queued', progress: 0 }), { status: 202 }));
		const job = await requestExport('demo', 'pdf', 'c1', 1, fetchFn as unknown as typeof fetch);
		expect(job.id).toBe('j');
		const [url, init] = fetchFn.mock.calls[0] as unknown as [string, RequestInit];
		expect(url).toBe('/api/boards/demo/exports');
		expect(JSON.parse(String(init.body))).toEqual({ format: 'pdf', scale: 1, client: 'c1' });
	});
	it('surfaces the server message on failure', async () => {
		const fetchFn = vi.fn(async () => new Response('export queue is full, try again shortly\n', { status: 503 }));
		await expect(requestExport('demo', 'png', 'c', 4, fetchFn as unknown as typeof fetch)).rejects.toThrow('queue is full');
		await expect(importBackup(new Blob(['{}']), fetchFn as unknown as typeof fetch)).rejects.toThrow('queue is full');
	});
	it('knows when a job is over', () => {
		expect(jobFinished({ id: '', board: '', format: 'png', state: 'running', progress: 5 })).toBe(false);
		expect(jobFinished({ id: '', board: '', format: 'png', state: 'canceled', progress: 5 })).toBe(true);
	});
});
