import { describe, expect, it } from 'vitest';
import { makeOp, parseEnvelope, roomSocketUrl } from './ws';

describe('roomSocketUrl', () => {
	it('uses ws for http and encodes the room', () => {
		const loc = { protocol: 'http:', host: 'localhost:5173' } as Location;
		expect(roomSocketUrl('my room', loc)).toBe('ws://localhost:5173/ws?room=my%20room');
	});

	it('uses wss for https', () => {
		const loc = { protocol: 'https:', host: 'board.example' } as Location;
		expect(roomSocketUrl('r', loc)).toBe('wss://board.example/ws?room=r');
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
