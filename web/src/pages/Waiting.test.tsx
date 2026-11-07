import { screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { fakeApi, untilLogout } from "../test/fakeApi";
import { overview, viewer, waiting } from "../test/fixtures";
import { renderApp } from "../test/render";

afterEach(() => vi.unstubAllGlobals());

describe("Waiting", () => {
  it("tells the account an admin needs to add it to a team", async () => {
    fakeApi({ "GET /api/me": { body: waiting } });
    renderApp("/");
    expect(
      await screen.findByText("You're signed in as new-user@example.com. An admin needs to add you to a team."),
    ).toBeInTheDocument();
  });

  it("asks /api/me again every 30 seconds, so an account added to a team gets in without a reload", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      let mes = 0;
      const fake = fakeApi({
        "GET /api/me": () => ({ body: mes++ === 0 ? waiting : viewer }),
        "GET /api/overview?days=7": { body: overview },
      });
      renderApp("/");
      expect(await screen.findByText(/An admin needs to add you to a team/)).toBeInTheDocument();
      expect(fake.find("GET", "/api/me")).toHaveLength(1);
      await vi.advanceTimersByTimeAsync(29_000);
      expect(fake.find("GET", "/api/me")).toHaveLength(1);
      await vi.advanceTimersByTimeAsync(2_000);
      expect(await screen.findByRole("navigation", { name: "Main" })).toBeInTheDocument();
      expect(fake.find("GET", "/api/me")).toHaveLength(2);
    } finally {
      vi.useRealTimers();
    }
  });

  it("shows the account's name as text, never as HTML", async () => {
    fakeApi({ "GET /api/me": { body: { ...waiting, email: "<b>bold</b>@example.com" } } });
    renderApp("/");
    expect(await screen.findByText(/<b>bold<\/b>/)).toBeInTheDocument();
    expect(document.querySelector("b")).toBeNull();
  });

  it("signs out with the CSRF token", async () => {
    const fake = fakeApi({
      ...untilLogout(waiting),
      "GET /api/sign-in-options": { body: { password: true, jwt: false } },
    });
    renderApp("/");
    await userEvent.click(await screen.findByRole("button", { name: "Sign out" }));
    expect(await screen.findByRole("heading", { name: "Sign in to pergent.sh" })).toBeInTheDocument();
    expect(fake.find("POST", "/auth/logout")[0].headers.get("X-CSRF-Token")).toBe("csrf-dev");
  });
});
