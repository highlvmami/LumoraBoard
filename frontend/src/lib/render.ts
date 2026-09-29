/**
 * Paints the board onto a 2D canvas. Pure drawing: no state, no events.
 * The whole board is redrawn per frame, which is fine for the object
 * counts a whiteboard sees; a layered canvas can come later if needed.
 */

import type { BoardObject, Patch, Point } from './board';
import { bounds, corners, STICKY_SIZE, TEXT_LINE, TEXT_SIZE, textSize, type Box } from './geometry';
import type { Viewport } from './viewport';

export const FONT = 'system-ui, -apple-system, "Segoe UI", sans-serif';
const DEFAULT_COLOR = '#1f2937';

export type DrawOptions = {
	selected?: string;
	/** Object being drawn locally and not sent yet (shape preview). */
	draft?: BoardObject | null;
	/** Live drag or resize of an object, overriding its own fields. */
	moving?: ({ id: string } & Patch) | null;
	/** Draw resize handles on the selection. */
	handles?: boolean;
	/** Object hidden while its text is being edited in place. */
	hidden?: string;
};

export function draw(
	ctx: CanvasRenderingContext2D,
	objects: BoardObject[],
	v: Viewport,
	size: { width: number; height: number; dpr: number },
	opts: DrawOptions = {}
): void {
	const { width, height, dpr } = size;
	ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
	ctx.clearRect(0, 0, width, height);
	drawGrid(ctx, v, width, height);

	ctx.setTransform(dpr * v.scale, 0, 0, dpr * v.scale, -v.x * v.scale * dpr, -v.y * v.scale * dpr);
	let selected: BoardObject | undefined;
	for (let o of objects) {
		if (o.id === opts.hidden) continue;
		if (opts.moving?.id === o.id) o = { ...o, ...opts.moving };
		drawObject(ctx, o);
		if (o.id === opts.selected) selected = o;
	}
	if (opts.draft) drawObject(ctx, opts.draft);

	if (selected) {
		const b = selectionBox(selected, v.scale);
		ctx.save();
		ctx.strokeStyle = '#6366f1';
		ctx.lineWidth = 1.5 / v.scale;
		if (!opts.handles) ctx.setLineDash([4 / v.scale, 3 / v.scale]);
		ctx.strokeRect(b.x, b.y, b.w, b.h);
		if (opts.handles) {
			const r = HANDLE / 2 / v.scale;
			ctx.fillStyle = 'white';
			for (const c of corners(b)) {
				ctx.fillRect(c.x - r, c.y - r, 2 * r, 2 * r);
				ctx.strokeRect(c.x - r, c.y - r, 2 * r, 2 * r);
			}
		}
		ctx.restore();
	}
}

/** Size of a resize handle, in screen pixels. */
export const HANDLE = 10;

/** The selection outline: the object's bounds plus a few screen pixels. */
export function selectionBox(o: BoardObject, scale: number): Box {
	const b = bounds(o);
	const pad = 4 / scale;
	return { x: b.x - pad, y: b.y - pad, w: b.w + 2 * pad, h: b.h + 2 * pad };
}

function drawGrid(ctx: CanvasRenderingContext2D, v: Viewport, width: number, height: number) {
	const step = 32 * v.scale;
	if (step < 10) return;
	ctx.fillStyle = '#d4d4d8';
	const r = Math.max(0.8, Math.min(1.5, v.scale));
	const startX = -((v.x * v.scale) % step);
	const startY = -((v.y * v.scale) % step);
	for (let x = startX; x < width; x += step) {
		for (let y = startY; y < height; y += step) {
			ctx.fillRect(x - r / 2, y - r / 2, r, r);
		}
	}
}

export function drawObject(ctx: CanvasRenderingContext2D, o: BoardObject): void {
	const color = o.color || DEFAULT_COLOR;
	ctx.save();
	ctx.strokeStyle = color;
	ctx.fillStyle = color;
	ctx.lineWidth = o.strokeWidth || 2;
	ctx.lineCap = 'round';
	ctx.lineJoin = 'round';

	switch (o.kind) {
		case 'stroke':
			drawStroke(ctx, o.x, o.y, o.points ?? []);
			break;
		case 'rect':
			ctx.strokeRect(o.x, o.y, o.w ?? 0, o.h ?? 0);
			break;
		case 'ellipse': {
			const rx = (o.w ?? 0) / 2;
			const ry = (o.h ?? 0) / 2;
			ctx.beginPath();
			ctx.ellipse(o.x + rx, o.y + ry, rx, ry, 0, 0, Math.PI * 2);
			ctx.stroke();
			break;
		}
		case 'arrow':
			drawArrow(ctx, o.x, o.y, o.points ?? []);
			break;
		case 'text':
			drawText(ctx, o.text ?? '', o.x, o.y, textSize(o));
			break;
		case 'sticky': {
			const w = o.w || STICKY_SIZE;
			const h = o.h || STICKY_SIZE;
			ctx.fillStyle = '#fde68a';
			ctx.shadowColor = 'rgba(0,0,0,0.15)';
			ctx.shadowBlur = 6;
			ctx.shadowOffsetY = 2;
			ctx.fillRect(o.x, o.y, w, h);
			ctx.shadowColor = 'transparent';
			ctx.fillStyle = DEFAULT_COLOR;
			drawText(ctx, o.text ?? '', o.x + 10, o.y + 10);
			break;
		}
	}
	ctx.restore();
}

function drawStroke(ctx: CanvasRenderingContext2D, ox: number, oy: number, pts: Point[]) {
	if (pts.length === 0) return;
	if (pts.length === 1) {
		ctx.beginPath();
		ctx.arc(ox + pts[0].x, oy + pts[0].y, ctx.lineWidth / 2, 0, Math.PI * 2);
		ctx.fill();
		return;
	}
	// Quadratic curves through segment midpoints smooth out the polyline
	// without needing extra points on the wire.
	ctx.beginPath();
	ctx.moveTo(ox + pts[0].x, oy + pts[0].y);
	for (let i = 1; i < pts.length - 1; i++) {
		const mx = (pts[i].x + pts[i + 1].x) / 2;
		const my = (pts[i].y + pts[i + 1].y) / 2;
		ctx.quadraticCurveTo(ox + pts[i].x, oy + pts[i].y, ox + mx, oy + my);
	}
	const last = pts[pts.length - 1];
	ctx.lineTo(ox + last.x, oy + last.y);
	ctx.stroke();
}

function drawArrow(ctx: CanvasRenderingContext2D, ox: number, oy: number, pts: Point[]) {
	if (pts.length < 2) return;
	const a = pts[0];
	const b = pts[pts.length - 1];
	ctx.beginPath();
	ctx.moveTo(ox + a.x, oy + a.y);
	ctx.lineTo(ox + b.x, oy + b.y);
	ctx.stroke();

	const angle = Math.atan2(b.y - a.y, b.x - a.x);
	const head = Math.max(10, ctx.lineWidth * 4);
	ctx.beginPath();
	ctx.moveTo(ox + b.x, oy + b.y);
	ctx.lineTo(ox + b.x - head * Math.cos(angle - Math.PI / 7), oy + b.y - head * Math.sin(angle - Math.PI / 7));
	ctx.lineTo(ox + b.x - head * Math.cos(angle + Math.PI / 7), oy + b.y - head * Math.sin(angle + Math.PI / 7));
	ctx.closePath();
	ctx.fill();
}

function drawText(ctx: CanvasRenderingContext2D, text: string, x: number, y: number, size = TEXT_SIZE) {
	ctx.font = `${size}px ${FONT}`;
	ctx.textBaseline = 'top';
	text.split('\n').forEach((line, i) => ctx.fillText(line, x, y + i * size * TEXT_LINE));
}

/** Size of a text block in world units, for its bounds. */
export function measureText(ctx: CanvasRenderingContext2D, text: string, size = TEXT_SIZE): { w: number; h: number } {
	ctx.save();
	ctx.font = `${size}px ${FONT}`;
	const lines = text.split('\n');
	const w = Math.max(...lines.map((l) => ctx.measureText(l).width));
	ctx.restore();
	// Default-size text keeps a whole-number height (what textSize() reads
	// back as TEXT_SIZE); resized text keeps its exact height.
	const h = lines.length * size * TEXT_LINE;
	return { w: Math.ceil(w), h: size === TEXT_SIZE ? Math.ceil(h) : h };
}
