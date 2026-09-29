import { describe, expect, it } from 'vitest';
import { applyOp, BoardStore, sorted, type Board, type Op } from './board';
import type { Envelope } from './ws';

const rect = (id: string, x = 0): Op => ({ kind: 'add', id, object: { id, kind: 'rect', x, y: 0, z: 0 } });
const op = (seq: number, from: string, payload: Op, clientOpId?: string): Envelope => ({
	v: 1,
	type: 'op',
	seq,
	from,
	clientOpId,
	payload
});
const hello = (clientId: string, seq: number, objects: unknown[] = [], resume = false): Envelope => ({
	v: 1,
	type: 'hello',
	seq,
	payload: { clientId, seq, members: [], resume, objects }
});

describe('applyOp', () => {
	it('adds, updates, reorders and deletes', () => {
		const b: Board = new Map();
		expect(applyOp(b, rect('a', 1), 1, 'me')).toBe(true);
		expect(b.get('a')).toMatchObject({ x: 1, createdBy: 'me', version: 1 });

		expect(applyOp(b, { kind: 'update', id: 'a', patch: { x: 9, text: 'hi' } }, 2, 'you')).toBe(true);
		expect(b.get('a')).toMatchObject({ x: 9, y: 0, text: 'hi', version: 2, createdBy: 'me' });

		expect(applyOp(b, { kind: 'reorder', id: 'a', z: 5 }, 3, 'you')).toBe(true);
		expect(b.get('a')?.z).toBe(5);

		expect(applyOp(b, { kind: 'delete', id: 'a' }, 4, 'you')).toBe(true);
		expect(b.size).toBe(0);
	});

	it('appends points only to strokes and arrows', () => {
		const b: Board = new Map();
		applyOp(b, { kind: 'add', id: 's', object: { id: 's', kind: 'stroke', x: 0, y: 0, z: 0, points: [{ x: 0, y: 0 }] } }, 1, 'me');
		expect(applyOp(b, { kind: 'append', id: 's', points: [{ x: 1, y: 1 }] }, 2, 'me')).toBe(true);
		expect(b.get('s')?.points).toHaveLength(2);

		applyOp(b, rect('r'), 3, 'me');
		expect(applyOp(b, { kind: 'append', id: 'r', points: [{ x: 1, y: 1 }] }, 4, 'me')).toBe(false);
	});

	it('rejects ops that do not fit without touching the board', () => {
		const b: Board = new Map();
		applyOp(b, rect('a'), 1, 'me');
		const before = JSON.stringify([...b]);
		expect(applyOp(b, rect('a'), 2, 'me')).toBe(false);
		expect(applyOp(b, { kind: 'update', id: 'nope', patch: { x: 1 } }, 2, 'me')).toBe(false);
		expect(applyOp(b, { kind: 'delete', id: 'nope' }, 2, 'me')).toBe(false);
		expect(JSON.stringify([...b])).toBe(before);
	});

	it('sorts by z then id', () => {
		const b: Board = new Map();
		applyOp(b, { kind: 'add', id: 'b', object: { id: 'b', kind: 'rect', x: 0, y: 0, z: 1 } }, 1, 'me');
		applyOp(b, { kind: 'add', id: 'a', object: { id: 'a', kind: 'rect', x: 0, y: 0, z: 1 } }, 2, 'me');
		applyOp(b, { kind: 'add', id: 'c', object: { id: 'c', kind: 'rect', x: 0, y: 0, z: 0 } }, 3, 'me');
		expect(sorted(b).map((o) => o.id)).toEqual(['c', 'a', 'b']);
	});
});

describe('BoardStore', () => {
	it('loads the snapshot from hello', () => {
		const s = new BoardStore();
		s.receive(hello('me', 7, [{ id: 'a', kind: 'rect', x: 1, y: 2, z: 0, version: 7 }]));
		expect(s.clientId).toBe('me');
		expect(s.seq).toBe(7);
		expect(s.view.get('a')?.x).toBe(1);
	});

	it('shows pending ops immediately and clears them on echo', () => {
		const s = new BoardStore();
		s.receive(hello('me', 0));
		s.local('c1', rect('a', 3));
		expect(s.view.get('a')?.x).toBe(3);
		expect(s.confirmed.has('a')).toBe(false);

		s.receive(op(1, 'me', rect('a', 3), 'c1'));
		expect(s.pending).toHaveLength(0);
		expect(s.confirmed.get('a')?.x).toBe(3);
		expect(s.seq).toBe(1);
	});

	it('replays pending ops over newer remote ops (last writer wins)', () => {
		const s = new BoardStore();
		s.receive(hello('me', 1, [{ id: 'a', kind: 'rect', x: 0, y: 0, z: 0, version: 1 }]));
		s.local('c1', { kind: 'update', id: 'a', patch: { x: 10 } });
		s.receive(op(2, 'you', { kind: 'update', id: 'a', patch: { x: 20 } }));
		// Locally the user still sees their own drag until the server orders it.
		expect(s.view.get('a')?.x).toBe(10);
		expect(s.confirmed.get('a')?.x).toBe(20);

		s.receive(op(3, 'me', { kind: 'update', id: 'a', patch: { x: 10 } }, 'c1'));
		expect(s.view.get('a')?.x).toBe(10);
		expect(s.confirmed.get('a')?.x).toBe(10);
	});

	it('drops a pending op the server rejects and reports it', () => {
		const events: unknown[] = [];
		const s = new BoardStore((e) => events.push(e));
		s.receive(hello('me', 0));
		s.local('c1', { kind: 'delete', id: 'ghost' });
		s.receive({ v: 1, type: 'reject', payload: { clientOpId: 'c1', reason: 'no object' } });
		expect(s.pending).toHaveLength(0);
		expect(events).toContainEqual({ type: 'rejected', clientOpId: 'c1', reason: 'no object' });
	});

	it('ignores ops at or below the current seq', () => {
		const s = new BoardStore();
		s.receive(hello('me', 2, [{ id: 'a', kind: 'rect', x: 5, y: 0, z: 0, version: 2 }]));
		expect(s.receive(op(2, 'you', { kind: 'update', id: 'a', patch: { x: 99 } }))).toBe(false);
		expect(s.view.get('a')?.x).toBe(5);
	});

	it('keeps the board on a resume hello and applies the replay', () => {
		const s = new BoardStore();
		s.receive(hello('me', 1, [{ id: 'a', kind: 'rect', x: 5, y: 0, z: 0, version: 1 }]));
		s.receive(hello('me2', 3, undefined, true));
		expect(s.view.get('a')?.x).toBe(5);
		expect(s.clientId).toBe('me2');
		s.receive(op(2, 'you', { kind: 'update', id: 'a', patch: { x: 6 } }));
		s.receive(op(3, 'you', rect('b')));
		expect(s.seq).toBe(3);
		expect(s.view.size).toBe(2);
	});

	it('tracks members', () => {
		const s = new BoardStore();
		s.receive({ v: 1, type: 'hello', payload: { clientId: 'me', seq: 0, members: ['x'], objects: [] } });
		s.receive({ v: 1, type: 'joined', from: 'y' });
		s.receive({ v: 1, type: 'left', from: 'x' });
		expect([...s.members]).toEqual(['y']);
	});
});
