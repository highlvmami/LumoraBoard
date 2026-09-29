/**
 * Minimal room client for the Faz 1 wire protocol. Reconnection, the
 * optimistic op store and typed board ops arrive in Faz 2 and 3.
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

export type ConnectionState = 'connecting' | 'open' | 'closed';

export type RoomEvents = {
	onMessage: (env: Envelope) => void;
	onState: (state: ConnectionState, detail?: string) => void;
};

/** Builds the URL of the socket endpoint relative to the current page. */
export function roomSocketUrl(room: string, location: Location = window.location): string {
	const scheme = location.protocol === 'https:' ? 'wss' : 'ws';
	return `${scheme}://${location.host}/ws?room=${encodeURIComponent(room)}`;
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

export type RoomConnection = {
	send: (payload: unknown) => Envelope;
	close: () => void;
};

/** Opens a socket to a room and reports frames and state changes. */
export function connectRoom(room: string, events: RoomEvents): RoomConnection {
	const socket = new WebSocket(roomSocketUrl(room));
	events.onState('connecting');

	socket.onopen = () => events.onState('open');
	socket.onclose = (e) => events.onState('closed', e.reason ? `${e.code} ${e.reason}` : `${e.code}`);
	socket.onerror = () => events.onState('closed', 'error');
	socket.onmessage = (e) => {
		const env = parseEnvelope(String(e.data));
		if (env) events.onMessage(env);
	};

	return {
		send(payload) {
			const env = makeOp(payload);
			socket.send(JSON.stringify(env));
			return env;
		},
		close() {
			socket.close(1000, 'client closed');
		}
	};
}
