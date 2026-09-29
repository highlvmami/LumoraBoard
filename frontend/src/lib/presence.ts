/**
 * Who else is in the room and where their pointers are.
 *
 * Presence is ephemeral: cursor messages carry no seq, the server may drop
 * them, and nothing here is persisted. A newer position always replaces
 * the older one, so a lost update only costs a little smoothness.
 */

import type { Envelope } from './ws';

export type Member = { id: string; name?: string };
export type Cursor = { x: number; y: number };

/** Palette cursor colours are picked from, chosen to read on white. */
const CURSOR_COLORS = ['#e11d48', '#2563eb', '#16a34a', '#d97706', '#7c3aed', '#0891b2', '#db2777', '#4d7c0f'];

/** Stable colour for a member id. */
export function colorFor(id: string): string {
	let h = 0;
	for (let i = 0; i < id.length; i++) h = (h * 31 + id.charCodeAt(i)) | 0;
	return CURSOR_COLORS[Math.abs(h) % CURSOR_COLORS.length];
}

/** What to show for a member: their name, or a short guest label. */
export function displayName(m: Member | undefined, id: string): string {
	return m?.name || `Guest ${id.slice(0, 4)}`;
}

export class Presence {
	members = new Map<string, Member>();
	cursors = new Map<string, Cursor>();

	/** Applies a server envelope. Returns true if anything visible changed. */
	receive(env: Envelope): boolean {
		switch (env.type) {
			case 'hello': {
				const h = env.payload as { members?: Member[] };
				this.members = new Map((h.members ?? []).map((m) => [m.id, m]));
				this.cursors = new Map();
				return true;
			}
			case 'joined': {
				if (!env.from) return false;
				const m = (env.payload as Member | undefined) ?? { id: env.from };
				this.members.set(env.from, { ...m, id: env.from });
				return true;
			}
			case 'left':
				if (!env.from) return false;
				this.members.delete(env.from);
				this.cursors.delete(env.from);
				return true;
			case 'cursor': {
				if (!env.from || !this.members.has(env.from)) return false;
				const c = env.payload as { x: number; y: number; hidden?: boolean };
				if (c.hidden) return this.cursors.delete(env.from);
				this.cursors.set(env.from, { x: c.x, y: c.y });
				return true;
			}
			default:
				return false;
		}
	}
}

/**
 * Rate-limits fn to one call per interval ms. The first call runs at once;
 * later calls inside the window collapse into one trailing call with the
 * latest arguments, so the final position is never lost.
 */
export function throttle<A extends unknown[]>(
	fn: (...args: A) => void,
	interval: number,
	now: () => number = () => Date.now(),
	schedule: (cb: () => void, ms: number) => unknown = setTimeout
): { call: (...args: A) => void; flush: () => void; cancel: () => void } {
	let last = -Infinity;
	let pending: A | null = null;
	let scheduled = false;

	function run() {
		scheduled = false;
		if (!pending) return;
		const args = pending;
		pending = null;
		last = now();
		fn(...args);
	}

	return {
		call(...args: A) {
			pending = args;
			const wait = last + interval - now();
			if (wait <= 0 && !scheduled) {
				run();
			} else if (!scheduled) {
				scheduled = true;
				schedule(run, wait);
			}
		},
		flush: run,
		cancel() {
			pending = null;
		}
	};
}
