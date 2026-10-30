import { screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { fakeApi } from "./test/fakeApi";
import { admin, developer, overview } from "./test/fixtures";
import { renderApp } from "./test/render";

afterEach(() => {
  vi.unstubAllGlobals();
  localStorage.clear();
});

const base = { "GET /api/overview?days=7": { body: overview } };

describe("App shell", () => {
  it("shows the menu with the Admin section to an admin", async () => {
    fakeApi({ ...base, "GET /api/me": { body: admin } });
    renderApp("/");
    const nav = await screen.findByRole("navigation", { name: "Main" });
    for (const label of ["Overview", "Repositories", "Reviews", "Skills", "Integrations", "Settings"]) {
      expect(within(nav).getByRole("link", { name: label })).toBeInTheDocument();
    }
    expect(nav).toHaveTextContent("Admin");
    expect(nav).not.toHaveTextContent("Organization");
    expect(screen.getByText(/platform-user/)).toHaveTextContent("platform-user · admin");
  });

  it("hides the Admin section from a developer", async () => {
    fakeApi({ ...base, "GET /api/me": { body: developer } });
    renderApp("/");
    const nav = await screen.findByRole("navigation", { name: "Main" });
    expect(within(nav).getByRole("link", { name: "Overview" })).toBeInTheDocument();
    expect(within(nav).queryByRole("link", { name: "Integrations" })).not.toBeInTheDocument();
    expect(within(nav).queryByRole("link", { name: "Settings" })).not.toBeInTheDocument();
    expect(nav).not.toHaveTextContent("Admin");
    expect(screen.getByText(/dev-user/)).toHaveTextContent("dev-user · developer");
  });

  it("answers a developer opening an admin page with not found", async () => {
    const fake = fakeApi({ ...base, "GET /api/me": { body: developer } });
    renderApp("/integrations");
    expect(await screen.findByRole("heading", { name: "Not found" })).toBeInTheDocument();
    expect(fake.find("GET", "/api/integrations")).toHaveLength(0);
  });

  it("marks the current page in the menu", async () => {
    fakeApi({ "GET /api/me": { body: admin }, "GET /api/repositories": { body: [] } });
    renderApp("/repos");
    expect(await screen.findByRole("link", { name: "Repositories" })).toHaveClass("active");
    expect(screen.getByRole("link", { name: "Overview" })).not.toHaveClass("active");
  });

  it("sends the user to sign-in when /api/me answers 401", async () => {
    fakeApi({ "GET /api/me": { status: 401, body: { error: "not signed in" } }, "GET /api/instances": { body: [] } });
    renderApp("/");
    expect(await screen.findByRole("heading", { name: "Sign in to pergent.sh" })).toBeInTheDocument();
    expect(screen.getByTestId("location")).toHaveTextContent(/^\/signin$/);
  });

  it("sends the user to sign-in when any call answers 401", async () => {
    fakeApi({
      "GET /api/me": { body: admin },
      "GET /api/overview?days=7": { status: 401, body: { error: "session expired" } },
      "GET /api/instances": { body: [] },
    });
    renderApp("/");
    expect(await screen.findByRole("heading", { name: "Sign in to pergent.sh" })).toBeInTheDocument();
  });

  it("shows a not found page for an unknown path", async () => {
    fakeApi({ "GET /api/me": { body: admin } });
    renderApp("/no/such/page");
    expect(await screen.findByRole("heading", { name: "Not found" })).toBeInTheDocument();
    expect(screen.getByText(/doesn't exist or you don't have access/)).toBeInTheDocument();
  });

  it("keeps nothing in localStorage", async () => {
    fakeApi({ ...base, "GET /api/me": { body: admin } });
    renderApp("/");
    await screen.findByText("184");
    expect(localStorage.length).toBe(0);
    expect(sessionStorage.length).toBe(0);
  });
});
