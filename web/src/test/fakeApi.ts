import { vi } from "vitest";

// A request the page made, as the fake API saw it.
export type Call = {
  method: string;
  url: string;
  headers: Headers;
  body: unknown;
};

export type Reply = { status?: number; body?: unknown };
export type Route = Reply | ((call: Call) => Reply);

// fakeApi replaces fetch with one that answers "METHOD /path?query" from
// routes, and records every call. A request no route answers gets 404, like
// the real API.
export function fakeApi(routes: Record<string, Route>) {
  const calls: Call[] = [];
  const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = typeof input === "string" ? input : input instanceof URL ? input.toString() : input.url;
    const method = (init?.method ?? "GET").toUpperCase();
    const call: Call = {
      method,
      url,
      headers: new Headers(init?.headers),
      body: typeof init?.body === "string" ? JSON.parse(init.body) : undefined,
    };
    calls.push(call);
    const route = routes[`${method} ${url}`];
    const reply: Reply = route === undefined ? { status: 404, body: { error: "not found" } } : typeof route === "function" ? route(call) : route;
    const status = reply.status ?? 200;
    return new Response(status === 204 ? null : JSON.stringify(reply.body ?? {}), {
      status,
      headers: { "Content-Type": "application/json" },
    });
  });
  vi.stubGlobal("fetch", fetchMock);
  return {
    calls,
    // find returns the calls of method to url.
    find: (method: string, url: string) => calls.filter((c) => c.method === method && c.url === url),
  };
}
