import { describe, expect, it } from 'vitest';
import type { BoardObject } from './board';
import { bounds, boxFrom, distToSegment, dragCorner, hits, MIN_SIZE, resizePatch, simplify, textSize, topmostAt } from './geometry';

const obj = (o: Partial<BoardObject> & Pick<BoardObject, 'id' | 'kind'>): BoardObject => ({
	x: 0,
	y: 0,
	z: 0,
	version: 1,
	...o
});

describe('geometry', () => {
	it('normalises boxes from any two corners', () => {
		expect(boxFrom({ x: 10, y: 5 }, { x: 2, y: 9 })).toEqual({ x: 2, y: 5, w: 8, h: 4 });
	});

	it('bounds strokes by their offset points plus half the width', () => {
		const s = obj({ id: 's', kind: 'stroke', x: 100, y: 100, strokeWidth: 4, points: [{ x: 0, y: 0 }, { x: 10, y: -20 }] });
		expect(bounds(s)).toEqual({ x: 98, y: 78, w: 14, h: 24 });
	});

	it('measures distance to a segment, clamped to its ends', () => {
		expect(distToSegment({ x: 5, y: 3 }, { x: 0, y: 0 }, { x: 10, y: 0 })).toBe(3);
		expect(distToSegment({ x: -3, y: 4 }, { x: 0, y: 0 }, { x: 10, y: 0 })).toBe(5);
	});

	it('hits strokes near the line only', () => {
		const s = obj({ id: 's', kind: 'stroke', x: 50, y: 50, strokeWidth: 2, points: [{ x: 0, y: 0 }, { x: 100, y: 0 }] });
		expect(hits(s, { x: 100, y: 53 }, 3)).toBe(true);
		expect(hits(s, { x: 100, y: 60 }, 3)).toBe(false);
	});

	it('hits rectangle and ellipse outlines, not their insides', () => {
		const r = obj({ id: 'r', kind: 'rect', x: 0, y: 0, w: 100, h: 50 });
		expect(hits(r, { x: 1, y: 25 }, 4)).toBe(true);
		expect(hits(r, { x: 50, y: 25 }, 4)).toBe(false);

		const e = obj({ id: 'e', kind: 'ellipse', x: 0, y: 0, w: 100, h: 50 });
		expect(hits(e, { x: 50, y: 1 }, 4)).toBe(true);
		expect(hits(e, { x: 50, y: 25 }, 4)).toBe(false);
	});

	it('treats text and sticky notes as solid', () => {
		const t = obj({ id: 't', kind: 'text', x: 0, y: 0, w: 80, h: 20 });
		expect(hits(t, { x: 40, y: 10 }, 0)).toBe(true);
		const n = obj({ id: 'n', kind: 'sticky', x: 0, y: 0 });
		expect(hits(n, { x: 150, y: 150 }, 0)).toBe(true);
	});

	it('picks the topmost hit', () => {
		const a = obj({ id: 'a', kind: 'text', w: 50, h: 50, z: 0 });
		const b = obj({ id: 'b', kind: 'text', w: 50, h: 50, z: 1 });
		expect(topmostAt([a, b], { x: 10, y: 10 }, 0)?.id).toBe('b');
		expect(topmostAt([a, b], { x: 90, y: 90 }, 0)).toBeUndefined();
	});

	it('simplifies dense points but keeps both ends', () => {
		const pts = [0, 0.5, 1, 1.5, 2, 5, 5.2].map((x) => ({ x, y: 0 }));
		expect(simplify(pts, 2).map((p) => p.x)).toEqual([0, 2, 5, 5.2]);
	});
});

describe('resizing', () => {
	const box = { x: 10, y: 20, w: 100, h: 50 };

	it('keeps the opposite corner fixed', () => {
		// Drag the bottom-right corner out; top-left stays at 10,20.
		expect(dragCorner(box, 2, { x: 210, y: 120 }, false)).toEqual({ x: 10, y: 20, w: 200, h: 100 });
		// Drag the top-left corner in; bottom-right stays at 110,70.
		expect(dragCorner(box, 0, { x: 60, y: 45 }, false)).toEqual({ x: 60, y: 45, w: 50, h: 25 });
	});

	it('does not flip past the fixed corner', () => {
		const b = dragCorner(box, 2, { x: -500, y: -500 }, false);
		expect(b).toEqual({ x: 10, y: 20, w: MIN_SIZE, h: MIN_SIZE });
	});

	it('keeps proportions when asked', () => {
		const b = dragCorner(box, 2, { x: 310, y: 30 }, true);
		expect(b.w / b.h).toBeCloseTo(2);
		expect(b.w).toBe(300);
	});

	it('scales stroke and arrow points', () => {
		const o = obj({ id: 'a', kind: 'arrow', x: 10, y: 20, points: [{ x: 0, y: 0 }, { x: 100, y: 50 }] });
		const p = resizePatch(o, box, { x: 10, y: 20, w: 200, h: 100 });
		expect(p).toEqual({ x: 10, y: 20, points: [{ x: 0, y: 0 }, { x: 200, y: 100 }] });
	});

	it('gives boxes the new size', () => {
		const o = obj({ id: 'r', kind: 'rect', x: 10, y: 20, w: 100, h: 50 });
		expect(resizePatch(o, box, { x: 0, y: 0, w: 30, h: 40 })).toEqual({ x: 0, y: 0, w: 30, h: 40 });
	});

	it('derives text size from the box height', () => {
		expect(textSize({ text: 'hi', h: 25 })).toBe(20);
		expect(textSize({ text: 'a\nb', h: 100 })).toBe(40);
		expect(textSize({ text: 'hi' })).toBe(20);
	});
});
