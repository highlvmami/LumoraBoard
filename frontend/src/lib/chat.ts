/**
 * Room chat on the client. The server numbers messages per board; the
 * newest ones arrive with hello, new ones as chat.message, and older pages
 * come from GET /api/boards/{board}/chat.
 */

import type { Envelope } from "./ws";

export type ChatMessage = {
  id: number;
  from: string;
  user?: string;
  name: string;
  avatar?: string;
  text: string;
  /** Board object the message is about, if any. */
  ref?: string;
  at: string;
};

export type ChatPage = { messages: ChatMessage[]; more: boolean };

/** Same limit as the server (proto.MaxChatRunes). */
export const MAX_CHAT_CHARS = 1000;

/** How long "is typing" shows after the last notice. */
export const TYPING_TTL = 3000;

/** Sorted, de-duplicated merge of two id-ordered lists. */
export function mergeMessages(
  a: ChatMessage[],
  b: ChatMessage[],
): ChatMessage[] {
  const byId = new Map<number, ChatMessage>();
  for (const m of a) byId.set(m.id, m);
  for (const m of b) byId.set(m.id, m);
  return [...byId.values()].sort((x, y) => x.id - y.id);
}

export class ChatStore {
  messages: ChatMessage[] = [];
  /** Older messages exist on the server than the first one here. */
  more = false;
  /** Who is typing, by connection id, with when the notice expires. */
  typing = new Map<string, number>();
  /** Messages this client sent that the server has not echoed yet. */
  private outstanding = new Set<string>();

  constructor(private readonly now: () => number = Date.now) {}

  /** Applies a server envelope. Returns true if anything visible changed. */
  receive(env: Envelope): boolean {
    switch (env.type) {
      case "hello": {
        const chat = (env.payload as { chat?: ChatPage }).chat;
        // Keep older pages already loaded: a reconnect only refreshes
        // the tail, and ids never change.
        const tail = chat?.messages ?? [];
        const keepOlder =
          this.messages.length > 0 &&
          tail.length > 0 &&
          this.messages[0].id < tail[0].id;
        this.messages = keepOlder
          ? mergeMessages(this.messages, tail)
          : tail.slice();
        if (!keepOlder) this.more = chat?.more ?? false;
        this.typing.clear();
        return true;
      }
      case "chat.message": {
        const m = env.payload as ChatMessage;
        if (env.clientOpId) this.outstanding.delete(env.clientOpId);
        const last = this.messages[this.messages.length - 1];
        this.messages =
          !last || last.id < m.id
            ? [...this.messages, m]
            : mergeMessages(this.messages, [m]);
        this.typing.delete(m.from);
        return true;
      }
      case "chat.typing":
        if (!env.from) return false;
        this.typing.set(env.from, this.now() + TYPING_TTL);
        return true;
      case "left":
        return env.from ? this.typing.delete(env.from) : false;
      default:
        return false;
    }
  }

  /** Remembers a sent message so its reject can be told apart from an op's. */
  sent(clientOpId: string): void {
    this.outstanding.add(clientOpId);
  }

  /** Claims a reject for a chat message this client sent. */
  claim(clientOpId: string): boolean {
    return this.outstanding.delete(clientOpId);
  }

  /** Adds an older page fetched over REST. */
  prepend(page: ChatPage): void {
    this.messages = mergeMessages(page.messages, this.messages);
    this.more = page.more;
  }

  /** Connection ids still typing; drops expired ones. */
  typers(): string[] {
    const now = this.now();
    for (const [id, until] of this.typing)
      if (until <= now) this.typing.delete(id);
    return [...this.typing.keys()];
  }
}

/** Fetches up to limit messages older than before. */
export async function fetchOlder(
  board: string,
  before: number,
  limit = 50,
  fetchFn: typeof fetch = fetch,
): Promise<ChatPage> {
  const q = new URLSearchParams({
    before: String(before),
    limit: String(limit),
  });
  const res = await fetchFn(
    `/api/boards/${encodeURIComponent(board)}/chat?${q}`,
    { credentials: "same-origin" },
  );
  if (!res.ok) throw new Error(`Could not load older messages (${res.status})`);
  return (await res.json()) as ChatPage;
}

/** "Ayşe is typing…", "Ayşe and Can are typing…", "3 people are typing…". */
export function typingLabel(names: string[]): string {
  if (names.length === 0) return "";
  if (names.length === 1) return `${names[0]} is typing…`;
  if (names.length === 2) return `${names[0]} and ${names[1]} are typing…`;
  return `${names.length} people are typing…`;
}
