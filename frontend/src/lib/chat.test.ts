import { describe, expect, it, vi } from "vitest";
import {
  ChatStore,
  fetchOlder,
  mergeMessages,
  TYPING_TTL,
  typingLabel,
  type ChatMessage,
} from "./chat";
import type { Envelope } from "./ws";

const msg = (id: number, from = "a"): ChatMessage => ({
  id,
  from,
  name: from.toUpperCase(),
  text: `m${id}`,
  at: "2026-09-29T10:00:00Z",
});
const env = (type: string, extra: Partial<Envelope> = {}): Envelope => ({
  v: 1,
  type,
  ...extra,
});

describe("ChatStore", () => {
  it("takes history from hello and appends new messages in order", () => {
    const s = new ChatStore();
    s.receive(
      env("hello", {
        payload: { chat: { messages: [msg(1), msg(2)], more: true } },
      }),
    );
    expect(s.more).toBe(true);
    s.receive(env("chat.message", { payload: msg(3) }));
    s.receive(env("chat.message", { payload: msg(3) })); // duplicate
    expect(s.messages.map((m) => m.id)).toEqual([1, 2, 3]);
  });

  it("keeps older pages across a reconnect", () => {
    const s = new ChatStore();
    s.receive(
      env("hello", {
        payload: { chat: { messages: [msg(51), msg(52)], more: true } },
      }),
    );
    s.prepend({ messages: [msg(49), msg(50)], more: false });
    s.receive(
      env("hello", {
        payload: { chat: { messages: [msg(52), msg(53)], more: true } },
      }),
    );
    expect(s.messages.map((m) => m.id)).toEqual([49, 50, 51, 52, 53]);
    expect(s.more).toBe(false);
  });

  it("tracks typing until it expires, a message arrives or the member leaves", () => {
    let now = 1000;
    const s = new ChatStore(() => now);
    s.receive(env("chat.typing", { from: "b" }));
    s.receive(env("chat.typing", { from: "c" }));
    expect(s.typers()).toEqual(["b", "c"]);
    s.receive(env("chat.message", { payload: msg(1, "b") }));
    s.receive(env("left", { from: "x" }));
    expect(s.typers()).toEqual(["c"]);
    now += TYPING_TTL;
    expect(s.typers()).toEqual([]);
  });

  it("claims rejects only for its own messages", () => {
    const s = new ChatStore();
    s.sent("m1");
    expect(s.claim("c1")).toBe(false);
    expect(s.claim("m1")).toBe(true);
    s.sent("m2");
    s.receive(env("chat.message", { clientOpId: "m2", payload: msg(1) }));
    expect(s.claim("m2")).toBe(false);
  });
});

describe("chat helpers", () => {
  it("merges without duplicates", () => {
    expect(
      mergeMessages([msg(1), msg(3)], [msg(2), msg(3)]).map((m) => m.id),
    ).toEqual([1, 2, 3]);
  });

  it("labels typing", () => {
    expect(typingLabel([])).toBe("");
    expect(typingLabel(["Ayşe"])).toBe("Ayşe is typing…");
    expect(typingLabel(["Ayşe", "Can"])).toBe("Ayşe and Can are typing…");
    expect(typingLabel(["a", "b", "c"])).toBe("3 people are typing…");
  });

  it("fetches older pages", async () => {
    const fetchFn = vi.fn(
      async () =>
        new Response(JSON.stringify({ messages: [msg(1)], more: false })),
    );
    const page = await fetchOlder(
      "demo",
      5,
      20,
      fetchFn as unknown as typeof fetch,
    );
    expect(page.messages).toHaveLength(1);
    expect(fetchFn).toHaveBeenCalledWith(
      "/api/boards/demo/chat?before=5&limit=20",
      { credentials: "same-origin" },
    );
    const failing = vi.fn(async () => new Response("no", { status: 403 }));
    await expect(
      fetchOlder("demo", 5, 20, failing as unknown as typeof fetch),
    ).rejects.toThrow("403");
  });
});
