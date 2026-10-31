import { afterEach, describe, expect, it, vi } from "vitest";

import { ApiError, api, isCSRFError, onGateChange, onUnauthorized, paths, safeHref } from "./api";
import { fakeApi } from "./test/fakeApi";

afterEach(() => vi.unstubAllGlobals());

describe("paths", () => {
  it("encodes each segment and keeps the project's slashes", () => {
    expect(paths.review("main", "acme/we ird?x", 5)).toBe("/reviews/main/acme/we%20ird%3Fx/5");
    expect(paths.repo("my instance", "acme/sub/widgets#1")).toBe("/repos/my%20instance/acme/sub/widgets%231");
    expect(paths.apiReview("main", "acme/payments", 77)).toBe("/api/reviews/main/acme/payments/77");
    expect(paths.apiRepo("main", "acme/payments")).toBe("/api/repositories/main/acme/payments");
    expect(paths.apiRun("a/b")).toBe("/api/runs/a%2Fb");
    expect(paths.login("gitlab", "main/../x")).toBe("/auth/gitlab/main%2F..%2Fx/login");
  });
});

describe("safeHref", () => {
  it("keeps only http and https addresses", () => {
    expect(safeHref("https://gitlab.example.com/acme/widgets")).toBe("https://gitlab.example.com/acme/widgets");
    expect(safeHref("http://gitlab.example.com/x")).toBe("http://gitlab.example.com/x");
    expect(safeHref("javascript:alert(1)")).toBeUndefined();
    expect(safeHref("data:text/html,hi")).toBeUndefined();
    expect(safeHref("//evil.example.com")).toBeUndefined();
    expect(safeHref("")).toBeUndefined();
  });
});

describe("api", () => {
  it("sends X-CSRF-Token and a JSON body on writes", async () => {
    const fake = fakeApi({ "PUT /api/repositories/main/acme/payments/settings": { body: { ok: true } } });
    await api.put("/api/repositories/main/acme/payments/settings", { approve: false }, "tok");
    const [call] = fake.calls;
    expect(call.headers.get("X-CSRF-Token")).toBe("tok");
    expect(call.headers.get("Content-Type")).toBe("application/json");
    expect(call.body).toEqual({ approve: false });
  });

  it("throws ApiError with the server's message", async () => {
    fakeApi({ "GET /api/overview": { status: 400, body: { error: "days must be a number" } } });
    const err = await api.get("/api/overview").catch((e: unknown) => e);
    expect(err).toBeInstanceOf(ApiError);
    expect((err as ApiError).status).toBe(400);
    expect((err as ApiError).message).toBe("days must be a number");
  });

  it("tells a CSRF refusal by its code, not its message", async () => {
    fakeApi({
      "PUT /api/a": { status: 403, body: { error: "the token is stale", code: "csrf" } },
      "PUT /api/b": { status: 403, body: { error: "missing or wrong CSRF token" } },
      "PUT /api/c": { status: 400, body: { error: "bad", code: "csrf" } },
    });
    const a = await api.put("/api/a", {}, "tok").catch((e: unknown) => e);
    expect(isCSRFError(a)).toBe(true);
    expect((a as ApiError).message).toBe("the token is stale");
    expect((a as ApiError).code).toBe("csrf");
    expect(isCSRFError(await api.put("/api/b", {}, "tok").catch((e: unknown) => e))).toBe(false);
    expect(isCSRFError(await api.put("/api/c", {}, "tok").catch((e: unknown) => e))).toBe(false);
  });

  it("tells the 401 listeners", async () => {
    fakeApi({ "GET /api/overview": { status: 401, body: { error: "not signed in" } } });
    const seen = vi.fn();
    const off = onUnauthorized(seen);
    await api.get("/api/overview").catch(() => undefined);
    off();
    expect(seen).toHaveBeenCalledTimes(1);
  });
});

describe("gate listeners", () => {
  it("are told of a 403 that says the account is waiting or must change its password", async () => {
    fakeApi({
      "GET /api/a": { status: 403, body: { error: "an admin needs to add you to a team", code: "waiting" } },
      "GET /api/b": { status: 403, body: { error: "change your password first", code: "password_change_required" } },
      "GET /api/c": { status: 403, body: { error: "forbidden" } },
      "GET /api/d": { status: 403, body: { error: "stale", code: "csrf" } },
      "GET /api/e": { status: 401, body: { error: "not signed in", code: "waiting" } },
    });
    const seen = vi.fn();
    const off = onGateChange(seen);
    await api.get("/api/a").catch(() => undefined);
    await api.get("/api/b").catch(() => undefined);
    expect(seen).toHaveBeenCalledTimes(2);
    await api.get("/api/c").catch(() => undefined);
    await api.get("/api/d").catch(() => undefined);
    await api.get("/api/e").catch(() => undefined);
    expect(seen).toHaveBeenCalledTimes(2);
    off();
    await api.get("/api/a").catch(() => undefined);
    expect(seen).toHaveBeenCalledTimes(2);
  });
});
