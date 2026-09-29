import { describe, expect, it } from 'vitest';
import { colorFor, displayName, Presence, throttle } from './presence';

describe('Presence', () => {
	it('tracks members from hello, joined and left', () => {
		const p = new Presence();
		p.receive({ v: 1, type: 'hello', payload: { clientId: 'me', seq: 0, members: [{ id: 'x', name: 'Ayşe' }] } });
		p.receive({ v: 1, type: 'joined', from: 'y', payload: { id: 'y', name: 'Bora' } });
		p.receive({ v: 1, type: 'left', from: 'x' });
		expect([...p.members.values()]).toEqual([{ id: 'y', name: 'Bora' }]);
	});

	it('moves, hides and forgets cursors', () => {
		const p = new Presence();
		p.receive({ v: 1, type: 'hello', payload: { members: [{ id: 'x' }] } });
		expect(p.receive({ v: 1, type: 'cursor', from: 'x', payload: { x: 1, y: 2 } })).toBe(true);
		p.receive({ v: 1, type: 'cursor', from: 'x', payload: { x: 5, y: 6 } });
		expect(p.cursors.get('x')).toEqual({ x: 5, y: 6 });

		p.receive({ v: 1, type: 'cursor', from: 'x', payload: { hidden: true } });
		expect(p.cursors.has('x')).toBe(false);

		p.receive({ v: 1, type: 'cursor', from: 'x', payload: { x: 1, y: 1 } });
		p.receive({ v: 1, type: 'left', from: 'x' });
		expect(p.cursors.size).toBe(0);
	});

	it('ignores cursors from unknown members', () => {
		const p = new Presence();
		expect(p.receive({ v: 1, type: 'cursor', from: 'ghost', payload: { x: 1, y: 1 } })).toBe(false);
	});

	it('names guests and colours ids stably', () => {
		expect(displayName({ id: 'abcdef', name: 'Can' }, 'abcdef')).toBe('Can');
		expect(displayName(undefined, 'abcdef')).toBe('Guest abcd');
		expect(colorFor('abc')).toBe(colorFor('abc'));
	});
});

describe('throttle', () => {
	it('runs the first call, then one trailing call with the latest args', () => {
		let t = 0;
		const timers: { at: number; cb: () => void }[] = [];
		const calls: number[] = [];
		const th = throttle(
			(n: number) => calls.push(n),
			40,
			() => t,
			(cb, ms) => timers.push({ at: t + ms, cb })
		);

		th.call(1);
		th.call(2);
		th.call(3);
		expect(calls).toEqual([1]);
		expect(timers).toHaveLength(1);

		t = 40;
		timers.shift()!.cb();
		expect(calls).toEqual([1, 3]);

		t = 100;
		th.call(4);
		expect(calls).toEqual([1, 3, 4]);
	});

	it('flush runs the pending call now; cancel drops it', () => {
		let t = 0;
		const calls: number[] = [];
		const th = throttle(
			(n: number) => calls.push(n),
			40,
			() => t,
			() => {}
		);
		th.call(1);
		th.call(2);
		th.flush();
		expect(calls).toEqual([1, 2]);
		th.call(3);
		th.cancel();
		th.flush();
		expect(calls).toEqual([1, 2]);
	});
});
