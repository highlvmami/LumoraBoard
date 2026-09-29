/**
 * Board export. PNG and SVG are made in the browser from the board as the
 * user sees it; high-resolution PNG, PDF and JSON backups are server jobs
 * whose progress arrives on the room socket.
 */

import type { BoardObject, Point } from './board';
import { bounds, STICKY_SIZE, TEXT_LINE, TEXT_SIZE, type Box } from './geometry';
import { drawObject, FONT } from './render';

const MARGIN = 32;
const DEFAULT_COLOR = '#1f2937';

/** Area an export covers: every object plus a margin, or a blank page. */
export function exportArea(objects: BoardObject[]): Box {
	if (objects.length === 0) return { x: 0, y: 0, w: 800, h: 600 };
	let minX = Infinity,
		minY = Infinity,
		maxX = -Infinity,
		maxY = -Infinity;
	for (const o of objects) {
		const b = bounds(o);
		minX = Math.min(minX, b.x);
		minY = Math.min(minY, b.y);
		maxX = Math.max(maxX, b.x + b.w);
		maxY = Math.max(maxY, b.y + b.h);
	}
	return { x: minX - MARGIN, y: minY - MARGIN, w: maxX - minX + 2 * MARGIN, h: maxY - minY + 2 * MARGIN };
}

const esc = (s: string) => s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');
const n = (v: number) => String(Math.round(v * 100) / 100);

/** Only #rgb/#rrggbb pass through to markup; anything else is the default ink. */
function safeColor(c: string | undefined): string {
	return c && /^#([0-9a-f]{3}|[0-9a-f]{6})$/i.test(c) ? c : DEFAULT_COLOR;
}

function strokePath(ox: number, oy: number, pts: Point[]): string {
	let d = `M${n(ox + pts[0].x)} ${n(oy + pts[0].y)}`;
	for (let i = 1; i < pts.length - 1; i++) {
		const mx = (pts[i].x + pts[i + 1].x) / 2;
		const my = (pts[i].y + pts[i + 1].y) / 2;
		d += ` Q${n(ox + pts[i].x)} ${n(oy + pts[i].y)} ${n(ox + mx)} ${n(oy + my)}`;
	}
	const last = pts[pts.length - 1];
	return `${d} L${n(ox + last.x)} ${n(oy + last.y)}`;
}

function textSvg(text: string, x: number, y: number, color: string): string {
	const lines = text.split('\n');
	const spans = lines
		.map((l, i) => `<tspan x="${n(x)}" y="${n(y + i * TEXT_SIZE * TEXT_LINE)}">${esc(l) || ' '}</tspan>`)
		.join('');
	return `<text font-size="${TEXT_SIZE}" dominant-baseline="hanging" fill="${color}" xml:space="preserve">${spans}</text>`;
}

function objectSvg(o: BoardObject): string {
	const c = safeColor(o.color);
	const w = o.strokeWidth || 2;
	const stroke = `fill="none" stroke="${c}" stroke-width="${n(w)}" stroke-linecap="round" stroke-linejoin="round"`;
	switch (o.kind) {
		case 'stroke': {
			const pts = o.points ?? [];
			if (pts.length === 0) return '';
			if (pts.length === 1) return `<circle cx="${n(o.x + pts[0].x)}" cy="${n(o.y + pts[0].y)}" r="${n(w / 2)}" fill="${c}"/>`;
			return `<path d="${strokePath(o.x, o.y, pts)}" ${stroke}/>`;
		}
		case 'rect':
			return `<rect x="${n(o.x)}" y="${n(o.y)}" width="${n(o.w ?? 0)}" height="${n(o.h ?? 0)}" ${stroke}/>`;
		case 'ellipse': {
			const rx = (o.w ?? 0) / 2;
			const ry = (o.h ?? 0) / 2;
			return `<ellipse cx="${n(o.x + rx)}" cy="${n(o.y + ry)}" rx="${n(rx)}" ry="${n(ry)}" ${stroke}/>`;
		}
		case 'arrow': {
			const pts = o.points ?? [];
			if (pts.length < 2) return '';
			const a = pts[0];
			const b = pts[pts.length - 1];
			const [bx, by] = [o.x + b.x, o.y + b.y];
			const angle = Math.atan2(b.y - a.y, b.x - a.x);
			const head = Math.max(10, w * 4);
			const p1 = `${n(bx - head * Math.cos(angle - Math.PI / 7))},${n(by - head * Math.sin(angle - Math.PI / 7))}`;
			const p2 = `${n(bx - head * Math.cos(angle + Math.PI / 7))},${n(by - head * Math.sin(angle + Math.PI / 7))}`;
			return (
				`<line x1="${n(o.x + a.x)}" y1="${n(o.y + a.y)}" x2="${n(bx)}" y2="${n(by)}" ${stroke}/>` +
				`<polygon points="${n(bx)},${n(by)} ${p1} ${p2}" fill="${c}"/>`
			);
		}
		case 'text':
			return textSvg(o.text ?? '', o.x, o.y, c);
		case 'sticky': {
			const sw = o.w || STICKY_SIZE;
			const sh = o.h || STICKY_SIZE;
			return (
				`<rect x="${n(o.x)}" y="${n(o.y)}" width="${n(sw)}" height="${n(sh)}" fill="#fde68a"/>` +
				textSvg(o.text ?? '', o.x + 10, o.y + 10, DEFAULT_COLOR)
			);
		}
	}
}

/** The board as a standalone SVG document. */
export function toSVG(objects: BoardObject[]): string {
	const a = exportArea(objects);
	const body = objects.map(objectSvg).join('\n');
	return (
		`<svg xmlns="http://www.w3.org/2000/svg" viewBox="${n(a.x)} ${n(a.y)} ${n(a.w)} ${n(a.h)}" ` +
		`width="${Math.ceil(a.w)}" height="${Math.ceil(a.h)}" font-family="${esc(FONT)}">\n` +
		`<rect x="${n(a.x)}" y="${n(a.y)}" width="${n(a.w)}" height="${n(a.h)}" fill="#fff"/>\n${body}\n</svg>\n`
	);
}

/** The board as a PNG drawn by the same code as the screen. */
export async function toPNG(objects: BoardObject[], scale = 2): Promise<Blob> {
	const a = exportArea(objects);
	// Browsers refuse canvases much past 16k per side.
	scale = Math.min(scale, 16_000 / a.w, 16_000 / a.h);
	const canvas = document.createElement('canvas');
	canvas.width = Math.max(1, Math.floor(a.w * scale));
	canvas.height = Math.max(1, Math.floor(a.h * scale));
	const ctx = canvas.getContext('2d');
	if (!ctx) throw new Error('Canvas is not available');
	ctx.fillStyle = '#fff';
	ctx.fillRect(0, 0, canvas.width, canvas.height);
	ctx.setTransform(scale, 0, 0, scale, -a.x * scale, -a.y * scale);
	for (const o of objects) drawObject(ctx, o);
	return new Promise((resolve, reject) =>
		canvas.toBlob((b) => (b ? resolve(b) : reject(new Error('Could not encode PNG'))), 'image/png')
	);
}

/** Saves a blob or URL under a file name. */
export function download(what: Blob | string, filename: string): void {
	const url = typeof what === 'string' ? what : URL.createObjectURL(what);
	const link = document.createElement('a');
	link.href = url;
	link.download = filename;
	document.body.append(link);
	link.click();
	link.remove();
	if (typeof what !== 'string') setTimeout(() => URL.revokeObjectURL(url), 10_000);
}

// ---- server jobs ------------------------------------------------------

export type ServerFormat = 'png' | 'pdf' | 'json';
export type JobState = 'queued' | 'running' | 'done' | 'failed' | 'canceled';

export type ExportJob = {
	id: string;
	board: string;
	format: ServerFormat;
	state: JobState;
	progress: number;
	error?: string;
	size?: number;
	file?: string;
};

export const jobFinished = (j: ExportJob) => j.state === 'done' || j.state === 'failed' || j.state === 'canceled';

async function failure(res: Response): Promise<Error> {
	const text = (await res.text()).trim();
	return new Error(text || `Request failed (${res.status})`);
}

export async function requestExport(
	board: string,
	format: ServerFormat,
	client: string,
	scale = 1,
	fetchFn: typeof fetch = fetch
): Promise<ExportJob> {
	const res = await fetchFn(`/api/boards/${encodeURIComponent(board)}/exports`, {
		method: 'POST',
		credentials: 'same-origin',
		headers: { 'Content-Type': 'application/json' },
		body: JSON.stringify({ format, scale, client })
	});
	if (!res.ok) throw await failure(res);
	return (await res.json()) as ExportJob;
}

export async function exportStatus(id: string, fetchFn: typeof fetch = fetch): Promise<ExportJob> {
	const res = await fetchFn(`/api/exports/${encodeURIComponent(id)}`, { credentials: 'same-origin' });
	if (!res.ok) throw await failure(res);
	return (await res.json()) as ExportJob;
}

export async function cancelExport(id: string, fetchFn: typeof fetch = fetch): Promise<void> {
	await fetchFn(`/api/exports/${encodeURIComponent(id)}`, { method: 'DELETE', credentials: 'same-origin' });
}

/** Uploads a JSON backup and returns the new board's name. */
export async function importBackup(file: Blob, fetchFn: typeof fetch = fetch): Promise<{ board: string; objects: number }> {
	const res = await fetchFn('/api/boards/import', {
		method: 'POST',
		credentials: 'same-origin',
		headers: { 'Content-Type': 'application/json' },
		body: file
	});
	if (!res.ok) throw await failure(res);
	return (await res.json()) as { board: string; objects: number };
}
