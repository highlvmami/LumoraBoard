/**
 * Room socket client: envelope helpers plus a connection that reconnects
 * with exponential backoff and resumes from the last seq it saw.
 */

export const PROTOCOL_VERSION = 1;

export type Envelope = {
	v: number;
	type: string;
	room?: string;
	seq?: number;
	from?: string;
	clientOpId?: string;
	payload?: unknown;
};

export type ConnectionState = 'connecting' | 'open' | 'reconnecting' | 'closed';

export type RoomEvents = {
	onMessage: (env: Envelope) => void;
	/** code is the close code when the state change came from a close. */
	onState: (state: ConnectionState, detail?: string, code?: number) => void;
};

export type ConnectOptions = {
	/** Last seq the caller has applied; sent as `since` so the server can replay instead of snapshot. */
	since?: () => number;
	/** Display name other members see. */
	name?: string;
	/** Backoff bounds in ms. */
	minDelay?: number;
	maxDelay?: number;
	/** Injectable for tests. */
	createSocket?: (url: string) => WebSocket;
	random?: () => number;
};

/** Builds the URL of the socket endpoint relative to the current page. */
export function roomSocketUrl(room: string, since = 0, location: Location = globalThis.location, name = ''): string {
	const scheme = location.protocol === 'https:' ? 'wss' : 'ws';
	let url = `${scheme}://${location.host}/ws?room=${encodeURIComponent(room)}`;
	if (since > 0) url += `&since=${since}`;
	if (name) url += `&name=${encodeURIComponent(name)}`;
	return url;
}

/** Parses a server frame; returns null for anything that is not an envelope. */
export function parseEnvelope(raw: string): Envelope | null {
	try {
		const env = JSON.parse(raw);
		if (typeof env !== 'object' || env === null) return null;
		if (env.v !== PROTOCOL_VERSION || typeof env.type !== 'string') return null;
		return env as Envelope;
	} catch {
		return null;
	}
}

let opCounter = 0;

/** Wraps a payload in an op envelope with a fresh client op id. */
export function makeOp(payload: unknown): Envelope {
	opCounter += 1;
	return { v: PROTOCOL_VERSION, type: 'op', clientOpId: `c${Date.now()}-${opCounter}`, payload };
}

/**
 * Backoff delay for the n-th consecutive failure (n starts at 0): doubling
 * from min up to max, with full jitter so reconnecting clients spread out.
 */
export function backoffDelay(attempt: number, min: number, max: number, random: () => number = Math.random): number {
	const cap = Math.min(max, min * 2 ** attempt);
	return Math.floor(min + random() * Math.max(0, cap - min));
}

/** Server close codes for sign-in problems (see backend ws package). */
export const CLOSE_UNAUTHORIZED = 4401;
export const CLOSE_FORBIDDEN = 4403;

/** Close codes after which reconnecting would only repeat the failure. */
const FATAL_CLOSE_CODES = new Set([1003, 1008, CLOSE_UNAUTHORIZED, CLOSE_FORBIDDEN]);

export type RoomConnection = {
	/** Sends an op; returns the envelope so the caller can track its clientOpId. Throws if not open. */
	send: (payload: unknown) => Envelope;
	/** Sends a presence update. Best effort: dropped silently while not open. */
	sendCursor: (payload: { x: number; y: number } | { hidden: true }) => void;
	close: () => void;
};

/** Opens a socket to a room, reconnecting until `close` is called. */
export function connectRoom(room: string, events: RoomEvents, opts: ConnectOptions = {}): RoomConnection {
	const minDelay = opts.minDelay ?? 500;
	const maxDelay = opts.maxDelay ?? 15_000;
	const createSocket = opts.createSocket ?? ((url) => new WebSocket(url));
	const random = opts.random ?? Math.random;

	let socket: WebSocket | null = null;
	let closed = false;
	let attempt = 0;
	let timer: ReturnType<typeof setTimeout> | null = null;

	function open() {
		const since = opts.since?.() ?? 0;
		events.onState(attempt === 0 ? 'connecting' : 'reconnecting', attempt === 0 ? undefined : `attempt ${attempt}`);
		const s = createSocket(roomSocketUrl(room, since, globalThis.location, opts.name));
		socket = s;

		s.onopen = () => {
			attempt = 0;
			events.onState('open');
		};
		s.onmessage = (e) => {
			const env = parseEnvelope(String(e.data));
			if (env) events.onMessage(env);
		};
		s.onerror = () => {
			// onclose follows; nothing to do here.
		};
		s.onclose = (e) => {
			if (socket !== s) return;
			socket = null;
			const detail = e.reason ? `${e.code} ${e.reason}` : `${e.code}`;
			if (closed || FATAL_CLOSE_CODES.has(e.code)) {
				events.onState('closed', detail, e.code);
				return;
			}
			const delay = backoffDelay(attempt, minDelay, maxDelay, random);
			attempt += 1;
			events.onState('reconnecting', `${detail}, retry in ${delay} ms`, e.code);
			timer = setTimeout(open, delay);
		};
	}

	open();

	return {
		send(payload) {
			if (!socket || socket.readyState !== WebSocket.OPEN) {
				throw new Error('socket is not open');
			}
			const env = makeOp(payload);
			socket.send(JSON.stringify(env));
			return env;
		},
		sendCursor(payload) {
			if (!socket || socket.readyState !== WebSocket.OPEN) return;
			socket.send(JSON.stringify({ v: PROTOCOL_VERSION, type: 'cursor', payload }));
		},
		close() {
			closed = true;
			if (timer) clearTimeout(timer);
			socket?.close(1000, 'client closed');
			if (!socket) events.onState('closed', 'client closed');
		}
	};
}
