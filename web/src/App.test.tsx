import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { fakeApi } from "./test/fakeApi";
import {
  maintainer,
  mustChange,
  operator,
  overview,
  platformAdmin,
  repoDetail,
  repoPayments,
  viewer,
  waiting,
} from "./test/fixtures";
import { renderApp } from "./test/render";

afterEach(() => {
  vi.unstubAllGlobals();
  localStorage.clear();
});

const base = { "GET /api/overview?days=7": { body: overview } };

describe("App shell", () => {
  it("shows the whole Admin section to an operator", async () => {
    fakeApi({ ...base, "GET /api/me": { body: operator } });
    renderApp("/");
    const nav = await screen.findByRole("navigation", { name: "Main" });
    for (const label of ["Overview", "Repositories", "Reviews", "Skills", "Users", "Teams", "Integrations", "Settings", "Audit log"]) {
      expect(within(nav).getByRole("link", { name: label })).toBeInTheDocument();
    }
    expect(nav).toHaveTextContent("Admin");
    expect(screen.getByText(/^admin ·/)).toHaveTextContent("admin · operator");
    expect(screen.getByText("All instances")).toBeInTheDocument();
  });

  it("shows the Admin section to a platform admin, with its instance", async () => {
    fakeApi({ ...base, "GET /api/me": { body: platformAdmin } });
    renderApp("/");
    const nav = await screen.findByRole("navigation", { name: "Main" });
    for (const label of ["Users", "Teams", "Integrations", "Settings", "Audit log"]) {
      expect(within(nav).getByRole("link", { name: label })).toBeInTheDocument();
    }
    expect(screen.getByText(/platform-user/)).toHaveTextContent("platform-user · admin");
    expect(screen.getByText("GitLab · main")).toBeInTheDocument();
  });

  it("badges Users with the accounts waiting for a team", async () => {
    fakeApi({ ...base, "GET /api/me": { body: { ...operator, waitingUsers: 3 } } });
    renderApp("/");
    const nav = await screen.findByRole("navigation", { name: "Main" });
    // The link's accessible name keeps the badge apart from the label.
    expect(within(nav).getByRole("link", { name: "Users, 3 waiting" })).toHaveTextContent("3 waiting");
    expect(within(nav).getByRole("link", { name: "Teams" })).not.toHaveTextContent("waiting");
  });

  it("shows the gate the server's 403 asks for, without a reload", async () => {
    let mes = 0;
    const fake = fakeApi({
      "GET /api/me": () => ({ body: mes++ === 0 ? platformAdmin : waiting }),
      "GET /api/overview?days=7": { status: 403, body: { error: "an admin needs to add you to a team", code: "waiting" } },
    });
    renderApp("/");
    expect(await screen.findByText("You're signed in as new-user on main. An admin needs to add you to a team.")).toBeInTheDocument();
    expect(fake.find("GET", "/api/me")).toHaveLength(2);
  });

  it("shows the password change when the server's 403 asks for it", async () => {
    let mes = 0;
    fakeApi({
      "GET /api/me": () => ({ body: mes++ === 0 ? operator : mustChange }),
      "GET /api/overview?days=7": { status: 403, body: { error: "change your password first", code: "password_change_required" } },
    });
    renderApp("/");
    expect(await screen.findByRole("heading", { name: "Choose a new password" })).toBeInTheDocument();
  });

  it("shows no badge when nobody is waiting", async () => {
    fakeApi({ ...base, "GET /api/me": { body: operator } });
    renderApp("/");
    const nav = await screen.findByRole("navigation", { name: "Main" });
    expect(within(nav).getByRole("link", { name: "Users" })).not.toHaveTextContent("waiting");
  });

  it("hides the Admin section from a viewer", async () => {
    fakeApi({ ...base, "GET /api/me": { body: viewer } });
    renderApp("/");
    const nav = await screen.findByRole("navigation", { name: "Main" });
    expect(within(nav).getByRole("link", { name: "Overview" })).toBeInTheDocument();
    for (const label of ["Users", "Teams", "Integrations", "Settings", "Audit log"]) {
      expect(within(nav).queryByRole("link", { name: label })).not.toBeInTheDocument();
    }
    expect(nav).not.toHaveTextContent("Admin");
    expect(screen.getByText(/dev-user/)).toHaveTextContent("dev-user · member");
  });

  it("answers a viewer opening an admin page with not found", async () => {
    const fake = fakeApi({ ...base, "GET /api/me": { body: viewer } });
    for (const path of ["/teams", "/users", "/audit", "/integrations"]) {
      const { unmount } = renderApp(path);
      expect(await screen.findByRole("heading", { name: "Not found" })).toBeInTheDocument();
      unmount();
    }
    expect(fake.find("GET", "/api/integrations")).toHaveLength(0);
  });

  it("shows a maintainer the Settings tab of a repository, but no on/off switch", async () => {
    fakeApi({
      "GET /api/me": { body: maintainer },
      "GET /api/repositories": { body: [repoPayments] },
      "GET /api/repositories/main/acme/payments": { body: repoDetail(repoPayments, "maintainer") },
      "GET /api/reviews?project=acme%2Fpayments&days=90": { body: { items: [], nextCursor: "" } },
    });
    const first = renderApp("/repos");
    await screen.findByRole("link", { name: "acme/payments" });
    expect(screen.queryByRole("switch")).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "+ Add repository" })).not.toBeInTheDocument();
    expect(screen.getByText(/^maint-user/)).toHaveTextContent("maint-user · member");
    first.unmount();

    renderApp("/repos/main/acme/payments");
    expect(await screen.findByRole("tab", { name: "Settings" })).toBeInTheDocument();
  });

  it("shows only the password change to an account that must change its password", async () => {
    const fake = fakeApi({ "GET /api/me": { body: mustChange } });
    renderApp("/");
    expect(await screen.findByRole("heading", { name: "Choose a new password" })).toBeInTheDocument();
    expect(screen.queryByRole("navigation", { name: "Main" })).not.toBeInTheDocument();
    // Nothing else is asked of the server, which would refuse it.
    expect(fake.calls.map((c) => c.url)).toEqual(["/api/me"]);
  });

  it("shows only the waiting page to an account on no team", async () => {
    const fake = fakeApi({ "GET /api/me": { body: waiting } });
    renderApp("/repos");
    expect(await screen.findByText("You're signed in as new-user on main. An admin needs to add you to a team.")).toBeInTheDocument();
    expect(screen.queryByRole("navigation", { name: "Main" })).not.toBeInTheDocument();
    expect(fake.calls.map((c) => c.url)).toEqual(["/api/me"]);
  });

  it("marks the current page in the menu", async () => {
    fakeApi({ "GET /api/me": { body: platformAdmin }, "GET /api/repositories": { body: [] } });
    renderApp("/repos");
    expect(await screen.findByRole("link", { name: "Repositories" })).toHaveClass("active");
    expect(screen.getByRole("link", { name: "Overview" })).not.toHaveClass("active");
  });

  it("sends the user to sign-in when /api/me answers 401", async () => {
    fakeApi({ "GET /api/me": { status: 401, body: { error: "not signed in" } }, "GET /api/instances": { body: { password: true, instances: [] } } });
    renderApp("/");
    expect(await screen.findByRole("heading", { name: "Sign in to pergent.sh" })).toBeInTheDocument();
    expect(screen.getByTestId("location")).toHaveTextContent(/^\/signin$/);
  });

  it("sends the user to sign-in when any call answers 401", async () => {
    fakeApi({
      "GET /api/me": { body: platformAdmin },
      "GET /api/overview?days=7": { status: 401, body: { error: "session expired" } },
      "GET /api/instances": { body: { password: true, instances: [] } },
    });
    renderApp("/");
    expect(await screen.findByRole("heading", { name: "Sign in to pergent.sh" })).toBeInTheDocument();
  });

  it("shows a not found page for an unknown path", async () => {
    fakeApi({ "GET /api/me": { body: platformAdmin } });
    renderApp("/no/such/page");
    expect(await screen.findByRole("heading", { name: "Not found" })).toBeInTheDocument();
    expect(screen.getByText(/doesn't exist or you don't have access/)).toBeInTheDocument();
  });

  it("keeps nothing in localStorage", async () => {
    fakeApi({ ...base, "GET /api/me": { body: platformAdmin } });
    renderApp("/");
    await screen.findByText("184");
    expect(localStorage.length).toBe(0);
    expect(sessionStorage.length).toBe(0);
  });

  it("signs out only when the server ended the session, retrying once with a fresh CSRF token", async () => {
    let logouts = 0;
    let mes = 0;
    const fake = fakeApi({
      ...base,
      "GET /api/me": () => ({ body: mes++ === 0 ? platformAdmin : { ...platformAdmin, csrfToken: "csrf-new" } }),
      "POST /auth/logout": () =>
        logouts++ === 0 ? { status: 403, body: { error: "missing or wrong CSRF token", code: "csrf" } } : { status: 204 },
      "GET /api/instances": { body: { password: true, instances: [] } },
    });
    renderApp("/");
    await userEvent.click(await screen.findByRole("button", { name: "Sign out" }));
    expect(await screen.findByRole("heading", { name: "Sign in to pergent.sh" })).toBeInTheDocument();
    const calls = fake.find("POST", "/auth/logout");
    expect(calls.map((c) => c.headers.get("X-CSRF-Token"))).toEqual(["csrf-123", "csrf-new"]);
    expect(fake.find("GET", "/api/me")).toHaveLength(2);
  });

  it("stays and says so when signing out fails", async () => {
    const fake = fakeApi({
      ...base,
      "GET /api/me": { body: platformAdmin },
      "POST /auth/logout": { status: 500, body: { error: "could not sign out" } },
    });
    renderApp("/");
    await userEvent.click(await screen.findByRole("button", { name: "Sign out" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("could not sign out");
    expect(screen.getByTestId("location")).toHaveTextContent(/^\/$/);
    expect(screen.getByRole("navigation", { name: "Main" })).toBeInTheDocument();
    expect(fake.find("POST", "/auth/logout")).toHaveLength(1);
  });
});
