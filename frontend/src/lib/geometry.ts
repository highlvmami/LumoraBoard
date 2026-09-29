/**
 * Shapes as geometry: bounding boxes and hit testing, used by the select
 * and eraser tools. Stroke and arrow points are relative to the object's
 * x/y, so moving any object is a single x/y update.
 */

import type { BoardObject, Patch, Point } from './board';

export type Box = { x: number; y: number; w: number; h: number };

/** Font size of text objects, in world units. */
export const TEXT_SIZE = 20;
export const TEXT_LINE = 1.25;
export const STICKY_SIZE = 160;

/**
 * Font size of a text object. It follows the box height, so resizing the
 * box scales the text; text that was never resized has
 * h = lines * TEXT_SIZE * TEXT_LINE and comes out at TEXT_SIZE.
 */
export function textSize(o: Pick<BoardObject, 'text' | 'h'>): number {
	const lines = (o.text ?? '').split('\n').length;
	return o.h && o.h > 0 ? o.h / (lines * TEXT_LINE) : TEXT_SIZE;
}

/** Smallest box a resize may produce, in world units. */
export const MIN_SIZE = 8;

/** Corners of a box, clockwise from top-left. */
export function corners(b: Box): Point[] {
	return [
		{ x: b.x, y: b.y },
		{ x: b.x + b.w, y: b.y },
		{ x: b.x + b.w, y: b.y + b.h },
		{ x: b.x, y: b.y + b.h }
	];
}

/**
 * The box a corner drag produces: the opposite corner stays put and the
 * dragged one follows p, without flipping past the fixed corner. With
 * keepAspect the box keeps from's proportions.
 */
export function dragCorner(from: Box, corner: number, p: Point, keepAspect: boolean): Box {
	const anchor = corners(from)[(corner + 2) % 4];
	const sx = corner === 1 || corner === 2 ? 1 : -1;
	const sy = corner === 2 || corner === 3 ? 1 : -1;
	let w = Math.max(MIN_SIZE, (p.x - anchor.x) * sx);
	let h = Math.max(MIN_SIZE, (p.y - anchor.y) * sy);
	if (keepAspect && from.w > 0 && from.h > 0) {
		const k = Math.max(w / from.w, h / from.h, MIN_SIZE / Math.min(from.w, from.h));
		w = from.w * k;
		h = from.h * k;
	}
	return { x: sx > 0 ? anchor.x : anchor.x - w, y: sy > 0 ? anchor.y : anchor.y - h, w, h };
}

/** Whether resizing o should keep its proportions (text scales as a whole). */
export const keepsAspect = (o: BoardObject) => o.kind === 'text';

/**
 * The patch that fits o into box to, where from is its current bounds().
 * Strokes and arrows scale their points; boxes take the new size.
 */
export function resizePatch(o: BoardObject, from: Box, to: Box): Patch {
	// A flat line (w or h near 0) cannot stretch along that axis.
	const sx = from.w > 1 ? to.w / from.w : 1;
	const sy = from.h > 1 ? to.h / from.h : 1;
	const map = (x: number, y: number) => ({ x: to.x + (x - from.x) * sx, y: to.y + (y - from.y) * sy });
	switch (o.kind) {
		case 'stroke':
		case 'arrow': {
			const at = map(o.x, o.y);
			return { ...at, points: (o.points ?? []).map((q) => ({ x: q.x * sx, y: q.y * sy })) };
		}
		default:
			return { x: to.x, y: to.y, w: to.w, h: to.h };
	}
}

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
