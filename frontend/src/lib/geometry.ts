/**
 * Shapes as geometry: bounding boxes and hit testing, used by the select
 * and eraser tools. Stroke and arrow points are relative to the object's
 * x/y, so moving any object is a single x/y update.
 */

import type { BoardObject, Point } from './board';

export type Box = { x: number; y: number; w: number; h: number };

/** Font size of text objects, in world units. */
export const TEXT_SIZE = 20;
export const TEXT_LINE = 1.25;
export const STICKY_SIZE = 160;

/** Rect from two corners, with non-negative size. */
export function boxFrom(a: Point, b: Point): Box {
	return { x: Math.min(a.x, b.x), y: Math.min(a.y, b.y), w: Math.abs(b.x - a.x), h: Math.abs(b.y - a.y) };
}

/** Axis-aligned bounds of an object in world coordinates. */
export function bounds(o: BoardObject): Box {
	if ((o.kind === 'stroke' || o.kind === 'arrow') && o.points?.length) {
		let minX = Infinity,
			minY = Infinity,
			maxX = -Infinity,
			maxY = -Infinity;
		for (const p of o.points) {
			minX = Math.min(minX, p.x);
			minY = Math.min(minY, p.y);
			maxX = Math.max(maxX, p.x);
			maxY = Math.max(maxY, p.y);
		}
		const pad = (o.strokeWidth ?? 2) / 2;
		return { x: o.x + minX - pad, y: o.y + minY - pad, w: maxX - minX + 2 * pad, h: maxY - minY + 2 * pad };
	}
	if (o.kind === 'sticky') return { x: o.x, y: o.y, w: o.w || STICKY_SIZE, h: o.h || STICKY_SIZE };
	return { x: o.x, y: o.y, w: o.w ?? 0, h: o.h ?? 0 };
}

/** Distance from p to the segment ab. */
export function distToSegment(p: Point, a: Point, b: Point): number {
	const dx = b.x - a.x;
	const dy = b.y - a.y;
	const len2 = dx * dx + dy * dy;
	const t = len2 === 0 ? 0 : Math.max(0, Math.min(1, ((p.x - a.x) * dx + (p.y - a.y) * dy) / len2));
	return Math.hypot(p.x - (a.x + t * dx), p.y - (a.y + t * dy));
}

/**
 * Whether world point p touches o, within tol world units. Outlines count
 * for shapes (so you can pick things behind a rectangle); text and sticky
 * notes are solid.
 */
export function hits(o: BoardObject, p: Point, tol: number): boolean {
	switch (o.kind) {
		case 'stroke':
		case 'arrow': {
			const pts = o.points ?? [];
			const reach = tol + (o.strokeWidth ?? 2) / 2;
			const local = { x: p.x - o.x, y: p.y - o.y };
			if (pts.length === 1) return Math.hypot(local.x - pts[0].x, local.y - pts[0].y) <= reach;
			for (let i = 1; i < pts.length; i++) {
				if (distToSegment(local, pts[i - 1], pts[i]) <= reach) return true;
			}
			return false;
		}
		case 'rect': {
			const b = bounds(o);
			const inOuter = p.x >= b.x - tol && p.x <= b.x + b.w + tol && p.y >= b.y - tol && p.y <= b.y + b.h + tol;
			const inInner = p.x > b.x + tol && p.x < b.x + b.w - tol && p.y > b.y + tol && p.y < b.y + b.h - tol;
			return inOuter && !inInner;
		}
		case 'ellipse': {
			const b = bounds(o);
			const rx = b.w / 2;
			const ry = b.h / 2;
			if (rx === 0 || ry === 0) return false;
			const cx = b.x + rx;
			const cy = b.y + ry;
			// Normalised radius: 1 on the outline. Scale tol by the
			// smaller radius so thin ellipses stay pickable.
			const r = Math.hypot((p.x - cx) / rx, (p.y - cy) / ry);
			return Math.abs(r - 1) * Math.min(rx, ry) <= tol;
		}
		case 'text':
		case 'sticky': {
			const b = bounds(o);
			return p.x >= b.x - tol && p.x <= b.x + b.w + tol && p.y >= b.y - tol && p.y <= b.y + b.h + tol;
		}
	}
}

/** Topmost object under p, given objects in paint order. */
export function topmostAt(objects: BoardObject[], p: Point, tol: number): BoardObject | undefined {
	for (let i = objects.length - 1; i >= 0; i--) {
		if (hits(objects[i], p, tol)) return objects[i];
	}
	return undefined;
}

/**
 * Drops points closer than minDist to the previous kept one. Pointer
 * events fire far more often than a stroke needs, and every point costs
 * bandwidth for everyone in the room.
 */
export function simplify(points: Point[], minDist: number): Point[] {
	if (points.length < 3) return points.slice();
	const out = [points[0]];
	for (let i = 1; i < points.length - 1; i++) {
		const last = out[out.length - 1];
		if (Math.hypot(points[i].x - last.x, points[i].y - last.y) >= minDist) out.push(points[i]);
	}
	out.push(points[points.length - 1]);
	return out;
}
