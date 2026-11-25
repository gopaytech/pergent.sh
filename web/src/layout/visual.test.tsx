import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { fakeApi } from "../test/fakeApi";
import { builtInRules, operator, overview, ruleSets, ssoAdmin, userDev, userOperator, userWaiting, viewer } from "../test/fixtures";
import { renderApp } from "../test/render";

afterEach(() => vi.unstubAllGlobals());

describe("the shell", () => {
  it("draws the menu's icons as SVG, not emoji", async () => {
    fakeApi({ "GET /api/me": { body: ssoAdmin }, "GET /api/overview?days=7": { body: overview } });
    renderApp("/");
    const nav = await screen.findByRole("navigation", { name: "Main" });
    expect(nav.querySelectorAll("svg").length).toBeGreaterThan(5);
    expect(nav).not.toHaveTextContent(/[\u{1F300}-\u{1FAFF}]/u);
  });

  it("has a Menu button that opens and closes the menu", async () => {
    fakeApi({ "GET /api/me": { body: ssoAdmin }, "GET /api/overview?days=7": { body: overview } });
    renderApp("/");
    const button = await screen.findByRole("button", { name: "Menu" });
    expect(button).toHaveAttribute("aria-expanded", "false");
    await userEvent.click(button);
    expect(button).toHaveAttribute("aria-expanded", "true");
    expect(document.querySelector(".shell")).toHaveClass("menu-open");
  });

  it("names the browser tab after the page", async () => {
    fakeApi({ "GET /api/me": { body: ssoAdmin }, "GET /api/overview?days=7": { body: overview } });
    renderApp("/");
    await waitFor(() => expect(document.title).toBe("Overview · pergent.sh"));
  });
});

describe("errors and times", () => {
  it("offers Try again when a page's read fails, and reads it again", async () => {
    let calls = 0;
    fakeApi({
      "GET /api/me": { body: ssoAdmin },
      "GET /api/overview?days=7": () => (calls++ === 0 ? { status: 500, body: { error: "boom" } } : { body: overview }),
    });
    renderApp("/");
    await userEvent.click(await screen.findByRole("button", { name: "Try again" }));
    expect(await screen.findByRole("region", { name: "Recent reviews" })).toBeInTheDocument();
  });

  it("shows the exact time of a relative one on hover", async () => {
    fakeApi({ "GET /api/me": { body: operator }, "GET /api/users": { body: [userWaiting, userDev, userOperator] } });
    renderApp("/admin/users");
    const when = (await screen.findAllByText(/ago$/))[0].closest("time") as HTMLElement;
    expect(when).toHaveAttribute("title", expect.stringMatching(/^\d{4}-\d{2}-\d{2} \d{2}:\d{2} UTC$/));
    expect(when).toHaveAttribute("datetime");
  });
});

describe("tabs", () => {
  it("move with the arrow keys, and only the chosen tab is in the Tab order", async () => {
    fakeApi({ "GET /api/me": { body: viewer }, "GET /api/rule-sets": { body: ruleSets }, "GET /api/built-in-rules": { body: builtInRules } });
    renderApp("/rules");
    const sets = await screen.findByRole("tab", { name: "Rule sets" });
    const builtIn = screen.getByRole("tab", { name: "Built-in" });
    expect(sets).toHaveAttribute("tabindex", "0");
    expect(builtIn).toHaveAttribute("tabindex", "-1");
    sets.focus();
    await userEvent.keyboard("{ArrowRight}");
    expect(builtIn).toHaveAttribute("aria-selected", "true");
    expect(builtIn).toHaveFocus();
    await userEvent.keyboard("{Home}");
    expect(sets).toHaveAttribute("aria-selected", "true");
  });
});
