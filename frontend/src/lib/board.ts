/**
 * Board model and the client-side sync store.
 *
 * The server assigns every accepted op a sequence number and the room
 * applies ops in that order, so replaying the op stream in seq order gives
 * every client the same board (last writer wins per object). The store
 * keeps that confirmed board plus the local ops the server has not echoed
 * back yet, and renders confirmed + pending so the user's own edits show
 * up immediately.
 */

import type { Envelope } from './ws';

export type ObjectKind = 'stroke' | 'rect' | 'ellipse' | 'arrow' | 'text' | 'sticky';

export type Point = { x: number; y: number };

export type BoardObject = {
	id: string;
	kind: ObjectKind;
	x: number;
	y: number;
	w?: number;
	h?: number;
	points?: Point[];
	text?: string;
	color?: string;
	strokeWidth?: number;
	z: number;
	createdBy?: string;
	version: number;
};

export type Patch = Partial<Pick<BoardObject, 'x' | 'y' | 'w' | 'h' | 'points' | 'text' | 'color' | 'strokeWidth'>>;

export type Op =
	| { kind: 'add'; id: string; object: Omit<BoardObject, 'version' | 'createdBy'> }
	| { kind: 'update'; id: string; patch: Patch }
	| { kind: 'delete'; id: string }
	| { kind: 'reorder'; id: string; z: number }
	| { kind: 'append'; id: string; points: Point[] };

export type Board = Map<string, BoardObject>;

/**
 * Applies one op to a board, mutating it. Returns false and leaves the
 * board untouched when the op does not fit (same rules as the server).
 */
export function applyOp(board: Board, op: Op, seq: number, from: string): boolean {
	switch (op.kind) {
		case 'add': {
			if (board.has(op.id)) return false;
			board.set(op.id, { ...op.object, points: op.object.points?.slice(), createdBy: from, version: seq });
			return true;
		}
		case 'update': {
			const o = board.get(op.id);
			if (!o) return false;
			const next = { ...o, ...op.patch, version: seq };
			if (op.patch.points) next.points = op.patch.points.slice();
			board.set(op.id, next);
			return true;
		}
		case 'delete':
			return board.delete(op.id);
		case 'reorder': {
			const o = board.get(op.id);
			if (!o) return false;
			board.set(op.id, { ...o, z: op.z, version: seq });
			return true;
		}
		case 'append': {
			const o = board.get(op.id);
			if (!o || (o.kind !== 'stroke' && o.kind !== 'arrow')) return false;
			board.set(op.id, { ...o, points: [...(o.points ?? []), ...op.points], version: seq });
			return true;
		}
	}
}

/** Objects in paint order: by z, then id, matching the server's snapshot. */
export function sorted(board: Board): BoardObject[] {
	return [...board.values()].sort((a, b) => a.z - b.z || (a.id < b.id ? -1 : a.id > b.id ? 1 : 0));
}

type Pending = { clientOpId: string; op: Op };

export type StoreEvent =
	| { type: 'reset' }
	| { type: 'rejected'; clientOpId: string; reason: string };

/**
 * Sync state for one room. Feed it every server envelope through
 * `receive`, and every local op through `local` after sending it.
 */
export class BoardStore {
	/** Board as the server has confirmed it. */
	confirmed: Board = new Map();
	/** Last seq applied to `confirmed`; sent as `since` on reconnect. */
	seq = 0;
	clientId = '';
	members = new Set<string>();
	pending: Pending[] = [];

	constructor(private readonly onEvent: (e: StoreEvent) => void = () => {}) {}

	/** Confirmed board with the pending local ops replayed on top. */
	get view(): Board {
		if (this.pending.length === 0) return this.confirmed;
		const view = new Map(this.confirmed);
		for (const p of this.pending) applyOp(view, p.op, this.seq, this.clientId);
		return view;
	}

	/** Records a local op the caller has just sent. */
	local(clientOpId: string, op: Op): void {
		this.pending.push({ clientOpId, op });
	}

	/** Applies a server envelope. Returns true if the board changed. */
	receive(env: Envelope): boolean {
		switch (env.type) {
			case 'hello':
				return this.hello(env);
			case 'op':
				return this.op(env);
			case 'reject': {
				const payload = env.payload as { clientOpId: string; reason: string };
				this.pending = this.pending.filter((p) => p.clientOpId !== payload.clientOpId);
				this.onEvent({ type: 'rejected', clientOpId: payload.clientOpId, reason: payload.reason });
				return true;
			}
			case 'joined':
				if (env.from) this.members.add(env.from);
				return false;
			case 'left':
				if (env.from) this.members.delete(env.from);
				return false;
			default:
				return false;
		}
	}

	private hello(env: Envelope): boolean {
		const h = env.payload as {
			clientId: string;
			seq: number;
			members: string[];
			resume?: boolean;
			objects?: BoardObject[];
		};
		this.clientId = h.clientId;
		this.members = new Set(h.members);
		if (h.resume) {
			// The ops we missed follow as ordinary op messages.
			return false;
		}
		this.confirmed = new Map((h.objects ?? []).map((o) => [o.id, o]));
		this.seq = h.seq;
		// A snapshot supersedes anything we had in flight on the old socket.
		this.pending = [];
		this.onEvent({ type: 'reset' });
		return true;
	}

	private op(env: Envelope): boolean {
		if (env.seq === undefined || env.seq <= this.seq) return false;
		const op = env.payload as Op;
		applyOp(this.confirmed, op, env.seq, env.from ?? '');
		this.seq = env.seq;
		if (env.from === this.clientId && env.clientOpId) {
			this.pending = this.pending.filter((p) => p.clientOpId !== env.clientOpId);
		}
		return true;
	}
}
