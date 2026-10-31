import { screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { fakeApi } from "../test/fakeApi";
import { waiting } from "../test/fixtures";
import { renderApp } from "../test/render";

afterEach(() => vi.unstubAllGlobals());

describe("Waiting", () => {
  it("tells the account an admin needs to add it to a team", async () => {
    fakeApi({ "GET /api/me": { body: waiting } });
    renderApp("/");
    expect(
      await screen.findByText("You're signed in as new-user on main. An admin needs to add you to a team."),
    ).toBeInTheDocument();
  });

  it("shows the account's name as text, never as HTML", async () => {
    fakeApi({ "GET /api/me": { body: { ...waiting, username: "<b>bold</b>" } } });
    renderApp("/");
    expect(await screen.findByText(/<b>bold<\/b>/)).toBeInTheDocument();
    expect(document.querySelector("b")).toBeNull();
  });

  it("signs out with the CSRF token", async () => {
    const fake = fakeApi({
      "GET /api/me": { body: waiting },
      "POST /auth/logout": { status: 204 },
      "GET /api/instances": { body: { password: true, instances: [] } },
    });
    renderApp("/");
    await userEvent.click(await screen.findByRole("button", { name: "Sign out" }));
    expect(await screen.findByRole("heading", { name: "Sign in to pergent.sh" })).toBeInTheDocument();
    expect(fake.find("POST", "/auth/logout")[0].headers.get("X-CSRF-Token")).toBe("csrf-dev");
  });
});
