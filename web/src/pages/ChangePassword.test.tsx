import { screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { fakeApi } from "../test/fakeApi";
import { mustChange, operator, overview } from "../test/fixtures";
import { renderApp } from "../test/render";

afterEach(() => vi.unstubAllGlobals());

async function fill(current: string, next: string, again = next) {
  await userEvent.type(await screen.findByLabelText("Current password"), current);
  await userEvent.type(screen.getByLabelText("New password"), next);
  await userEvent.type(screen.getByLabelText("New password again"), again);
  await userEvent.click(screen.getByRole("button", { name: "Change password" }));
}

describe("ChangePassword", () => {
  it("posts the current and the new password with the session's CSRF token, then opens the dashboard", async () => {
    let mes = 0;
    const fake = fakeApi({
      "GET /api/me": () => ({ body: mes++ === 0 ? mustChange : operator }),
      "POST /auth/password/change": { status: 204 },
      "GET /api/overview?days=7": { body: overview },
    });
    renderApp("/");
    await fill("generated-password-1234", "a much better password");
    expect(await screen.findByRole("navigation", { name: "Main" })).toBeInTheDocument();
    const [post] = fake.find("POST", "/auth/password/change");
    expect(post.body).toEqual({ current: "generated-password-1234", new: "a much better password" });
    expect(post.headers.get("X-CSRF-Token")).toBe("csrf-123");
  });

  it("refuses a new password under 12 characters without asking the server", async () => {
    const fake = fakeApi({ "GET /api/me": { body: mustChange } });
    renderApp("/");
    await fill("generated-password-1234", "too short");
    expect(await screen.findByRole("alert")).toHaveTextContent("at least 12 characters");
    expect(fake.find("POST", "/auth/password/change")).toHaveLength(0);
  });

  it("counts characters, not bytes", async () => {
    const fake = fakeApi({ "GET /api/me": { body: mustChange }, "POST /auth/password/change": { status: 204 } });
    renderApp("/");
    // 12 characters, 24 bytes.
    await fill("generated-password-1234", "ééééééééééé");
    expect(await screen.findByRole("alert")).toHaveTextContent("at least 12 characters");
    expect(fake.find("POST", "/auth/password/change")).toHaveLength(0);
  });

  it("refuses two different new passwords", async () => {
    const fake = fakeApi({ "GET /api/me": { body: mustChange } });
    renderApp("/");
    await fill("generated-password-1234", "a much better password", "a much better passwore");
    expect(await screen.findByRole("alert")).toHaveTextContent("don't match");
    expect(fake.find("POST", "/auth/password/change")).toHaveLength(0);
  });

  it("shows the server's text for a wrong current password and stays signed in", async () => {
    fakeApi({
      "GET /api/me": { body: mustChange },
      "POST /auth/password/change": { status: 401, body: { error: "wrong password" } },
      "GET /api/instances": { body: { password: true, instances: [] } },
    });
    renderApp("/");
    await fill("not the password", "a much better password");
    expect(await screen.findByRole("alert")).toHaveTextContent("wrong password");
    // A 401 here is the wrong password, not an ended session: no trip to sign-in.
    expect(screen.getByRole("heading", { name: "Choose a new password" })).toBeInTheDocument();
    expect(screen.getByTestId("location")).toHaveTextContent(/^\/$/);
  });

  it("retries once with a fresh CSRF token", async () => {
    let posts = 0;
    let mes = 0;
    const fake = fakeApi({
      "GET /api/me": () => ({ body: mes++ === 0 ? mustChange : { ...mustChange, csrfToken: "csrf-new" } }),
      "POST /auth/password/change": () =>
        posts++ === 0 ? { status: 403, body: { error: "missing or wrong CSRF token", code: "csrf" } } : { status: 204 },
    });
    renderApp("/");
    await fill("generated-password-1234", "a much better password");
    await vi.waitFor(() => expect(fake.find("POST", "/auth/password/change")).toHaveLength(2));
    expect(fake.find("POST", "/auth/password/change").map((c) => c.headers.get("X-CSRF-Token"))).toEqual([
      "csrf-123",
      "csrf-new",
    ]);
  });

  it("lets the account sign out instead", async () => {
    fakeApi({
      "GET /api/me": { body: mustChange },
      "POST /auth/logout": { status: 204 },
      "GET /api/instances": { body: { password: true, instances: [] } },
    });
    renderApp("/");
    await userEvent.click(await screen.findByRole("button", { name: "Sign out" }));
    expect(await screen.findByRole("heading", { name: "Sign in to pergent.sh" })).toBeInTheDocument();
  });
});
