import { focusManager } from "@tanstack/react-query";
import { act, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { fakeApi, untilLogout } from "../test/fakeApi";
import { auditPage1, operator, overview, ssoAdmin, userDev, viewer } from "../test/fixtures";
import { renderApp } from "../test/render";

afterEach(() => {
  vi.unstubAllGlobals();
  localStorage.clear();
});

const users = [userDev];
const base = { "GET /api/overview?days=7": { body: overview } };
const adminPages = ["Users", "Teams", "Integrations", "Settings", "Usage", "Audit log"];
const userPages = ["Overview", "Repositories", "Reviews", "Skills"];

// linkNames are the menu's links by their label, without the icon and the badge.
const linkNames = (nav: HTMLElement) =>
  within(nav)
    .getAllByRole("link")
    .map((a) => {
      const copy = a.cloneNode(true) as HTMLElement;
      copy.querySelectorAll(".icon, .badge").forEach((n) => n.remove());
      return (copy.textContent ?? "").trim();
    });

describe("Layout: the user area", () => {
  it("gives a user the user pages and nothing of the admin area", async () => {
    fakeApi({ ...base, "GET /api/me": { body: viewer } });
    renderApp("/");
    const nav = await screen.findByRole("navigation", { name: "Main" });
    expect(linkNames(nav)).toEqual(userPages);
    for (const label of [...adminPages, "Admin", "Admin area", "User pages"]) {
      expect(within(nav).queryByRole("link", { name: label })).not.toBeInTheDocument();
    }
    expect(screen.getByText(/dev-user/)).toHaveTextContent("dev-user · user");
  });

  it("gives an admin the user pages and a link across to the admin area", async () => {
    fakeApi({ ...base, "GET /api/me": { body: ssoAdmin } });
    renderApp("/");
    const nav = await screen.findByRole("navigation", { name: "Main" });
    expect(linkNames(nav)).toEqual([...userPages, "Admin area"]);
    expect(within(nav).getByRole("link", { name: "Admin area" })).toHaveAttribute("href", "/admin");
    for (const label of adminPages) expect(within(nav).queryByRole("link", { name: label })).not.toBeInTheDocument();
    expect(screen.getByText(/^sso-admin/)).toHaveTextContent("sso-admin · admin");
  });

  it("says in the link across how many users wait for a team", async () => {
    fakeApi({ ...base, "GET /api/me": { body: { ...operator, waitingUsers: 3 } } });
    renderApp("/");
    const nav = await screen.findByRole("navigation", { name: "Main" });
    expect(within(nav).getByRole("link", { name: "Admin area, 3 waiting" })).toHaveTextContent("3 waiting");
  });

  it("names the menu's account <name> · admin or <name> · user, and never says operator", async () => {
    fakeApi({ ...base, "GET /api/me": { body: operator } });
    const first = renderApp("/");
    await screen.findByRole("navigation", { name: "Main" });
    expect(screen.getByText(/^admin ·/)).toHaveTextContent("admin · admin");
    expect(document.body).not.toHaveTextContent(/operator/i);
    first.unmount();

    fakeApi({ ...base, "GET /api/me": { body: viewer } });
    renderApp("/");
    await screen.findByRole("navigation", { name: "Main" });
    expect(document.body).not.toHaveTextContent(/operator/i);
  });

  it("names no platform account or instance for the signed-in user", async () => {
    fakeApi({ ...base, "GET /api/me": { body: ssoAdmin } });
    renderApp("/");
    await screen.findByRole("navigation", { name: "Main" });
    expect(screen.queryByText("All instances")).not.toBeInTheDocument();
    expect(screen.queryByText(/GitLab · main|GitHub · /)).not.toBeInTheDocument();
  });

  it("marks the current page in the menu", async () => {
    fakeApi({ "GET /api/me": { body: ssoAdmin }, "GET /api/repositories": { body: [] } });
    renderApp("/repos");
    expect(await screen.findByRole("link", { name: "Repositories" })).toHaveClass("active");
    expect(screen.getByRole("link", { name: "Overview" })).not.toHaveClass("active");
  });
});

describe("Layout: the admin area", () => {
  it("gives an admin the admin pages and a link back to the user pages", async () => {
    fakeApi({ "GET /api/me": { body: operator }, "GET /api/users": { body: users } });
    renderApp("/admin/users");
    const nav = await screen.findByRole("navigation", { name: "Main" });
    expect(linkNames(nav)).toEqual(["Admin", ...adminPages, "User pages"]);
    expect(within(nav).getByRole("link", { name: "User pages" })).toHaveAttribute("href", "/");
    for (const label of ["Overview", "Repositories", "Reviews", "Skills"]) {
      expect(within(nav).queryByRole("link", { name: label })).not.toBeInTheDocument();
    }
    expect(within(nav).getByRole("link", { name: "Users" })).toHaveClass("active");
    expect(screen.getByText("Admin area")).toBeInTheDocument();
  });

  it("badges Users with the accounts waiting for a team", async () => {
    fakeApi({ "GET /api/me": { body: { ...operator, waitingUsers: 3 } }, "GET /api/users": { body: users } });
    renderApp("/admin/users");
    const nav = await screen.findByRole("navigation", { name: "Main" });
    // The link's accessible name keeps the badge apart from the label.
    expect(within(nav).getByRole("link", { name: "Users, 3 waiting" })).toHaveTextContent("3 waiting");
    expect(within(nav).getByRole("link", { name: "Teams" })).not.toHaveTextContent("waiting");
  });

  it("shows no badge when nobody is waiting", async () => {
    fakeApi({ "GET /api/me": { body: operator }, "GET /api/users": { body: users } });
    renderApp("/admin/users");
    const nav = await screen.findByRole("navigation", { name: "Main" });
    expect(within(nav).getByRole("link", { name: "Users" })).not.toHaveTextContent("waiting");
  });

  it("opens the admin pages at their /admin paths", async () => {
    fakeApi({ "GET /api/me": { body: operator }, "GET /api/audit": { body: auditPage1 } });
    renderApp("/admin/audit");
    expect(await screen.findByRole("heading", { name: "Audit log" })).toBeInTheDocument();
  });

  it("opens /admin on a page that lists the admin pages", async () => {
    fakeApi({ "GET /api/me": { body: { ...operator, waitingUsers: 2 } } });
    renderApp("/admin");
    expect(await screen.findByRole("heading", { name: "Admin" })).toBeInTheDocument();
    const pages = screen.getByRole("region", { name: "Admin pages" });
    expect(within(pages).getAllByRole("link").map((a) => a.getAttribute("href"))).toEqual([
      "/admin/users",
      "/admin/teams",
      "/admin/integrations",
      "/admin/settings",
      "/admin/usage",
      "/admin/audit",
    ]);
    expect(screen.getByRole("note")).toHaveTextContent("2 users are waiting for a team");
  });

  it("answers a user opening an admin page with not found, and asks the admin API nothing", async () => {
    const fake = fakeApi({ ...base, "GET /api/me": { body: viewer } });
    for (const path of ["/admin", "/admin/users", "/admin/teams", "/admin/audit", "/admin/integrations", "/admin/settings"]) {
      const { unmount } = renderApp(path);
      expect(await screen.findByRole("heading", { name: "Not found" })).toBeInTheDocument();
      // The user's menu, not the admin's.
      expect(linkNames(screen.getByRole("navigation", { name: "Main" }))).toEqual(userPages);
      unmount();
    }
    expect(fake.calls.map((c) => c.url).filter((u) => u !== "/api/me")).toEqual([]);
  });
});

describe("the old admin addresses", () => {
  it("send an admin to the page under /admin", async () => {
    fakeApi({ "GET /api/me": { body: operator } });
    for (const page of ["users", "teams", "integrations", "settings", "audit"]) {
      const { unmount } = renderApp(`/${page}`);
      await vi.waitFor(() => expect(screen.getByTestId("location")).toHaveTextContent(new RegExp(`^/admin/${page}$`)));
      unmount();
    }
  });

  it("are not found for a user, and ask the admin API nothing", async () => {
    const fake = fakeApi({ "GET /api/me": { body: viewer } });
    for (const page of ["users", "teams", "integrations", "settings", "audit"]) {
      const { unmount } = renderApp(`/${page}`);
      expect(await screen.findByRole("heading", { name: "Not found" })).toBeInTheDocument();
      expect(screen.getByTestId("location")).toHaveTextContent(new RegExp(`^/${page}$`));
      unmount();
    }
    expect(fake.calls.map((c) => c.url).filter((u) => u !== "/api/me")).toEqual([]);
  });
});

describe("signing out", () => {
  const options = { "GET /api/sign-in-options": { body: { password: true, jwt: false } } };

  // Dropped while the page is still up, /api/me would be read again by the
  // page on its way out: with JWT sign-in, that read can sign the person in.
  it("drops what it loaded only once the signed-in pages are gone", async () => {
    fakeApi({ ...base, ...options, ...untilLogout(ssoAdmin) });
    const { client } = renderApp("/");
    const clear = client.clear.bind(client);
    let buttonWhenCleared: HTMLElement | null | undefined;
    vi.spyOn(client, "clear").mockImplementation(() => {
      buttonWhenCleared = screen.queryByRole("button", { name: "Sign out" });
      clear();
    });
    await userEvent.click(await screen.findByRole("button", { name: "Sign out" }));
    await screen.findByRole("heading", { name: "Sign in to pergent.sh" });
    expect(client.clear).toHaveBeenCalledOnce();
    expect(buttonWhenCleared).toBeNull();
    expect(client.getQueryData(["me"])).toBeUndefined();
  });

  it("goes to the sign-in page from the user pages", async () => {
    fakeApi({ ...base, ...options, ...untilLogout(ssoAdmin) });
    renderApp("/");
    await userEvent.click(await screen.findByRole("button", { name: "Sign out" }));
    expect(await screen.findByRole("heading", { name: "Sign in to pergent.sh" })).toBeInTheDocument();
    expect(screen.getByTestId("location")).toHaveTextContent(/^\/sign-in$/);
  });

  it("goes to the admin sign-in page from the admin area", async () => {
    fakeApi({ ...options, ...untilLogout(operator), "GET /api/users": { body: users } });
    renderApp("/admin/users");
    await userEvent.click(await screen.findByRole("button", { name: "Sign out" }));
    expect(await screen.findByRole("heading", { name: "Admin sign-in" })).toBeInTheDocument();
    expect(screen.getByTestId("location")).toHaveTextContent(/^\/admin\/sign-in$/);
  });

  it("sends a local admin to the admin sign-in from the user pages too", async () => {
    fakeApi({ ...base, ...options, ...untilLogout(operator) });
    renderApp("/");
    await userEvent.click(await screen.findByRole("button", { name: "Sign out" }));
    expect(await screen.findByRole("heading", { name: "Admin sign-in" })).toBeInTheDocument();
    expect(screen.getByTestId("location")).toHaveTextContent(/^\/admin\/sign-in$/);
  });

  it("goes to /signed-out for a JWT session, and that page asks the server nothing", async () => {
    const fake = fakeApi({ ...base, ...options, ...untilLogout({ ...ssoAdmin, signIn: "jwt" }) });
    renderApp("/");
    await userEvent.click(await screen.findByRole("button", { name: "Sign out" }));
    expect(await screen.findByRole("heading", { name: "You're signed out" })).toBeInTheDocument();
    expect(screen.getByTestId("location")).toHaveTextContent(/^\/signed-out$/);
    // /api/me was read once, before the click: reading it again would sign the
    // person in again, the proxy's header being on the request.
    expect(fake.find("GET", "/api/me")).toHaveLength(1);
    expect(fake.find("GET", "/api/sign-in-options")).toHaveLength(0);
  });

  it("opens /signed-out without asking /api/me", async () => {
    const fake = fakeApi({});
    renderApp("/signed-out");
    expect(await screen.findByRole("heading", { name: "You're signed out" })).toBeInTheDocument();
    expect(fake.calls).toEqual([]);
    expect(screen.getByRole("link", { name: "Sign in again" })).toHaveAttribute("href", "/sign-in");
  });
});

describe("reading /api/me again", () => {
  // Focusing the window reads a stale query again. With JWT sign-in that would
  // sign the person in again after a sign-out in another tab, so it does not.
  const refocus = async (client: { invalidateQueries: (o: object) => Promise<void> }) => {
    await client.invalidateQueries({ queryKey: ["me"], refetchType: "none" });
    act(() => focusManager.setFocused(false));
    act(() => focusManager.setFocused(true));
  };

  it("does not happen on focus for a JWT session", async () => {
    const fake = fakeApi({ ...base, "GET /api/me": { body: { ...ssoAdmin, signIn: "jwt" } } });
    const { client } = renderApp("/");
    await screen.findByRole("navigation", { name: "Main" });
    await refocus(client);
    await new Promise((r) => setTimeout(r, 50));
    expect(fake.find("GET", "/api/me")).toHaveLength(1);
  });

  it("happens on focus for any other session", async () => {
    const fake = fakeApi({ ...base, "GET /api/me": { body: ssoAdmin } });
    const { client } = renderApp("/");
    await screen.findByRole("navigation", { name: "Main" });
    await refocus(client);
    await vi.waitFor(() => expect(fake.find("GET", "/api/me")).toHaveLength(2));
  });
});
