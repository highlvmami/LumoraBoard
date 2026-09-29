/**
 * Pan and zoom. A viewport maps board (world) coordinates to screen pixels:
 * screen = (world - origin) * scale. Everything on the wire is in world
 * coordinates, so two people can look at different parts of the board.
 */

import type { Point } from './board';

export type Viewport = { x: number; y: number; scale: number };

export const MIN_SCALE = 0.1;
export const MAX_SCALE = 8;

export const identity = (): Viewport => ({ x: 0, y: 0, scale: 1 });

export function toWorld(v: Viewport, p: Point): Point {
	return { x: p.x / v.scale + v.x, y: p.y / v.scale + v.y };
}

export function toScreen(v: Viewport, p: Point): Point {
	return { x: (p.x - v.x) * v.scale, y: (p.y - v.y) * v.scale };
}

/** Moves the view by a screen-space delta (dragging the board). */
export function panBy(v: Viewport, dx: number, dy: number): Viewport {
	return { ...v, x: v.x - dx / v.scale, y: v.y - dy / v.scale };
}

/**
 * Zooms by factor keeping the world point under the screen point `at`
 * fixed, which is what makes wheel and pinch zoom feel anchored.
 */
export function zoomAt(v: Viewport, at: Point, factor: number): Viewport {
	const scale = Math.min(MAX_SCALE, Math.max(MIN_SCALE, v.scale * factor));
	const anchor = toWorld(v, at);
	return { scale, x: anchor.x - at.x / scale, y: anchor.y - at.y / scale };
}

/** Pans (keeping the zoom) so the world point p sits in the middle of a screen of the given size. */
export function centerOn(v: Viewport, p: Point, size: { width: number; height: number }): Viewport {
	return { ...v, x: p.x - size.width / 2 / v.scale, y: p.y - size.height / 2 / v.scale };
}
