import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { backoffDelay, connectRoom, makeOp, parseEnvelope, roomSocketUrl, type ConnectionState } from './ws';

describe('roomSocketUrl', () => {
	it('uses ws for http and encodes the room', () => {
		const loc = { protocol: 'http:', host: 'localhost:5173' } as Location;
		expect(roomSocketUrl('my room', 0, loc)).toBe('ws://localhost:5173/ws?room=my%20room');
	});

	it('uses wss for https and appends since when resuming', () => {
		const loc = { protocol: 'https:', host: 'board.example' } as Location;
		expect(roomSocketUrl('r', 42, loc)).toBe('wss://board.example/ws?room=r&since=42');
	});

	it('appends the display name', () => {
		const loc = { protocol: 'http:', host: 'h' } as Location;
		expect(roomSocketUrl('r', 0, loc, 'Ayşe K')).toBe('ws://h/ws?room=r&name=Ay%C5%9Fe%20K');
	});
});

describe('parseEnvelope', () => {
	it('accepts a versioned envelope', () => {
		expect(parseEnvelope('{"v":1,"type":"op","seq":3}')).toEqual({ v: 1, type: 'op', seq: 3 });
	});

	it('rejects malformed or foreign frames', () => {
		expect(parseEnvelope('nope')).toBeNull();
		expect(parseEnvelope('42')).toBeNull();
		expect(parseEnvelope('{"v":2,"type":"op"}')).toBeNull();
		expect(parseEnvelope('{"v":1}')).toBeNull();
	});
});

describe('makeOp', () => {
	it('stamps a unique client op id', () => {
		const a = makeOp({ x: 1 });
		const b = makeOp({ x: 1 });
		expect(a.type).toBe('op');
		expect(a.v).toBe(1);
		expect(a.clientOpId).not.toBe(b.clientOpId);
	});
});

describe('backoffDelay', () => {
	it('doubles up to the cap and jitters within [min, cap]', () => {
		expect(backoffDelay(0, 100, 1000, () => 0)).toBe(100);
		expect(backoffDelay(0, 100, 1000, () => 1)).toBe(100);
		expect(backoffDelay(1, 100, 1000, () => 1)).toBe(200);
		expect(backoffDelay(3, 100, 1000, () => 1)).toBe(800);
		expect(backoffDelay(10, 100, 1000, () => 1)).toBe(1000);
		expect(backoffDelay(10, 100, 1000, () => 0.5)).toBe(550);
	});
});

/** Just enough of a WebSocket for the connection logic. */
class FakeSocket {
	static OPEN = 1;
	static instances: FakeSocket[] = [];
	readyState = 0;
	sent: string[] = [];
	onopen: (() => void) | null = null;
	onclose: ((e: { code: number; reason: string }) => void) | null = null;
	onerror: (() => void) | null = null;
	onmessage: ((e: { data: string }) => void) | null = null;
	constructor(public url: string) {
		FakeSocket.instances.push(this);
	}
	open() {
		this.readyState = 1;
		this.onopen?.();
	}
	serverClose(code: number, reason = '') {
		this.readyState = 3;
		this.onclose?.({ code, reason });
	}
	send(data: string) {
		this.sent.push(data);
	}
	close(code: number, reason: string) {
		this.serverClose(code, reason);
	}
}

describe('connectRoom', () => {
	beforeEach(() => {
		vi.useFakeTimers();
		FakeSocket.instances = [];
		vi.stubGlobal('WebSocket', FakeSocket);
		vi.stubGlobal('location', { protocol: 'http:', host: 'h' });
	});
	afterEach(() => {
		vi.useRealTimers();
		vi.unstubAllGlobals();
	});

	const states: ConnectionState[] = [];
	const events = { onMessage: vi.fn(), onState: (s: ConnectionState) => states.push(s) };
	const createSocket = (url: string) => new FakeSocket(url) as unknown as WebSocket;

	it('reconnects with backoff and resumes from the last seq', () => {
		states.length = 0;
		let seq = 0;
		connectRoom('r', events, { since: () => seq, minDelay: 100, maxDelay: 1000, random: () => 1, createSocket });

		const first = FakeSocket.instances[0];
		expect(first.url).toBe('ws://h/ws?room=r');
		first.open();
		seq = 5;
		first.serverClose(1006);
		expect(states).toEqual(['connecting', 'open', 'reconnecting']);
		expect(FakeSocket.instances).toHaveLength(1);

		vi.advanceTimersByTime(100);
		expect(FakeSocket.instances).toHaveLength(2);
		expect(FakeSocket.instances[1].url).toBe('ws://h/ws?room=r&since=5');

		// Second failure waits twice as long.
		FakeSocket.instances[1].serverClose(1006);
		vi.advanceTimersByTime(199);
		expect(FakeSocket.instances).toHaveLength(2);
		vi.advanceTimersByTime(1);
		expect(FakeSocket.instances).toHaveLength(3);
	});

	it('does not reconnect after a policy violation or a client close', () => {
		states.length = 0;
		const conn = connectRoom('r', events, { minDelay: 100, createSocket });
		FakeSocket.instances[0].open();
		FakeSocket.instances[0].serverClose(1008, 'slow consumer');
		vi.advanceTimersByTime(10_000);
		expect(FakeSocket.instances).toHaveLength(1);
		expect(states.at(-1)).toBe('closed');

		states.length = 0;
		const conn2 = connectRoom('r', events, { minDelay: 100, createSocket });
		FakeSocket.instances[1].open();
		conn2.close();
		vi.advanceTimersByTime(10_000);
		expect(FakeSocket.instances).toHaveLength(2);
		expect(states.at(-1)).toBe('closed');
		conn.close();
	});

	it('sends ops only while open', () => {
		const conn = connectRoom('r', events, { createSocket });
		expect(() => conn.send({ kind: 'delete', id: 'x' })).toThrow();
		FakeSocket.instances[0].open();
		const env = conn.send({ kind: 'delete', id: 'x' });
		expect(JSON.parse(FakeSocket.instances[0].sent[0])).toEqual(env);
		conn.close();
	});
});
