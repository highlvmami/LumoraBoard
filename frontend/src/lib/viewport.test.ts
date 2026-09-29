import { describe, expect, it } from 'vitest';
import { centerOn, identity, MAX_SCALE, MIN_SCALE, panBy, toScreen, toWorld, zoomAt } from './viewport';

describe('viewport', () => {
	it('round-trips between screen and world', () => {
		const v = { x: 100, y: -50, scale: 2 };
		const w = toWorld(v, { x: 40, y: 60 });
		expect(w).toEqual({ x: 120, y: -20 });
		expect(toScreen(v, w)).toEqual({ x: 40, y: 60 });
	});

	it('pans in screen pixels regardless of zoom', () => {
		const v = panBy({ x: 0, y: 0, scale: 2 }, 20, -10);
		expect(v).toEqual({ x: -10, y: 5, scale: 2 });
	});

	it('keeps the point under the cursor fixed when zooming', () => {
		const at = { x: 300, y: 200 };
		const before = toWorld(identity(), at);
		const v = zoomAt(identity(), at, 2.5);
		expect(v.scale).toBe(2.5);
		const after = toWorld(v, at);
		expect(after.x).toBeCloseTo(before.x);
		expect(after.y).toBeCloseTo(before.y);
	});

	it('clamps the scale', () => {
		expect(zoomAt(identity(), { x: 0, y: 0 }, 1000).scale).toBe(MAX_SCALE);
		expect(zoomAt(identity(), { x: 0, y: 0 }, 0.0001).scale).toBe(MIN_SCALE);
	});
});

describe('centerOn', () => {
	it('puts the point in the middle of the screen at the current zoom', () => {
		const v = centerOn({ x: 0, y: 0, scale: 2 }, { x: 100, y: 50 }, { width: 800, height: 600 });
		expect(toScreen(v, { x: 100, y: 50 })).toEqual({ x: 400, y: 300 });
		expect(v.scale).toBe(2);
	});
});
