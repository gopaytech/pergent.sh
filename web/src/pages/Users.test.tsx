import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { fakeApi } from "../test/fakeApi";
import {
  operator,
  signInNone,
  signInOIDC,
  teamEmpty,
  teamPayments,
  userDev,
  userInvited,
  userOperator,
  userWaiting,
} from "../test/fixtures";
import { renderApp } from "../test/render";

afterEach(() => vi.unstubAllGlobals());

// The API sends the waiting accounts first; the page keeps them there.
const users = [userDev, userOperator, userWaiting];

// The row whose first cell starts with name: a role in another cell can say "admin" too.
const rowOf = (name: string) => {
  const row = screen
    .getAllByRole("row")
    .slice(1)
    .find((r) => within(r).getAllByRole("cell")[0].textContent?.startsWith(name));
  if (!row) throw new Error(`no row for ${name}`);
  return row;
};
const findRow = async (name: string) => {
  await vi.waitFor(() => rowOf(name));
  return rowOf(name);
};

describe("Users", () => {
  it("lists the accounts with their sign-in, role, teams and last sign-in, the waiting ones first", async () => {
    fakeApi({ "GET /api/me": { body: operator }, "GET /api/users": { body: [...users, userInvited] } });
    renderApp("/admin/users");
    expect(await screen.findByRole("heading", { name: "Users" })).toBeInTheDocument();
    await screen.findByText("newcomer@example.com");
    expect(screen.getAllByRole("columnheader").map((h) => h.textContent)).toEqual([
      "User",
      "Sign-in",
      "Role",
      "Teams",
      "Last sign-in",
      "",
    ]);
    const rows = screen.getAllByRole("row").slice(1);
    expect(rows.map((r) => within(r).getAllByRole("cell")[0].textContent)).toEqual([
      "newcomer@example.comwaiting",
      "dev-user@example.com",
      "admin",
      "invited@example.cominvited",
    ]);
    expect(within(rowOf("newcomer@example.com")).getByText("waiting")).toHaveClass("pill", "warn");
    expect(within(rowOf("dev-user@example.com")).queryByText("waiting")).not.toBeInTheDocument();
    expect(rowOf("dev-user@example.com")).toHaveTextContent("Payments");
    expect(rowOf("dev-user@example.com")).toHaveTextContent("SSO");
    expect(rowOf("admin")).toHaveTextContent("Local");
    expect(rowOf("admin")).toHaveTextContent("never");
    // An invitation that never signed in: invited, not waiting.
    const invited = rowOf("invited@example.com");
    expect(within(invited).getByText("invited")).toHaveClass("pill");
    expect(within(invited).queryByText("waiting")).not.toBeInTheDocument();
    expect(invited).toHaveTextContent("never");
    expect(invited).toHaveTextContent("Payments");
  });

  it("has Add user and Add local admin, and no old name", async () => {
    fakeApi({ "GET /api/me": { body: operator }, "GET /api/users": { body: users } });
    renderApp("/admin/users");
    expect(await screen.findByRole("button", { name: "Add user" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Add local admin" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /password admin/i })).not.toBeInTheDocument();
    expect(document.body.textContent).not.toMatch(/operator|password admin|platform admin/i);
  });

  it("links to Integrations when no sign-in is set up, and says nothing when it is", async () => {
    fakeApi({
      "GET /api/me": { body: operator },
      "GET /api/users": { body: users },
      "GET /api/admin/sign-in": { body: signInNone },
    });
    const first = renderApp("/admin/users");
    const note = await screen.findByRole("note");
    expect(note).toHaveTextContent("Sign-in isn't set up");
    expect(within(note).getByRole("link", { name: /Integrations/ })).toHaveAttribute("href", "/admin/integrations");
    first.unmount();

    fakeApi({
      "GET /api/me": { body: operator },
      "GET /api/users": { body: users },
      "GET /api/admin/sign-in": { body: signInOIDC },
    });
    renderApp("/admin/users");
    await screen.findByText("newcomer@example.com");
    expect(screen.queryByRole("note")).not.toBeInTheDocument();
  });

  it("adds a user by email alone", async () => {
    let listed = 0;
    const fake = fakeApi({
      "GET /api/me": { body: operator },
      "GET /api/users": () => ({ body: listed++ === 0 ? users : [...users, userInvited] }),
      "GET /api/teams": { body: [teamPayments, teamEmpty] },
      "POST /api/admin/users": { status: 201, body: { ...userInvited, teams: [] } },
    });
    renderApp("/admin/users");
    await userEvent.click(await screen.findByRole("button", { name: "Add user" }));
    const form = screen.getByRole("form", { name: "Add user" });
    await userEvent.type(within(form).getByLabelText("Email"), " invited@example.com ");
    await userEvent.click(within(form).getByRole("button", { name: "Add" }));
    const [post] = fake.find("POST", "/api/admin/users");
    expect(post.body).toEqual({ email: "invited@example.com" });
    expect(post.headers.get("X-CSRF-Token")).toBe("csrf-123");
    // The form closes and the list shows the invitation.
    expect(await screen.findByText("invited")).toBeInTheDocument();
    expect(screen.queryByRole("form", { name: "Add user" })).not.toBeInTheDocument();
  });

  it("adds a user on teams, each with a role that starts as viewer", async () => {
    const fake = fakeApi({
      "GET /api/me": { body: operator },
      "GET /api/users": { body: users },
      "GET /api/teams": { body: [teamPayments, teamEmpty] },
      "POST /api/admin/users": { status: 201, body: userInvited },
    });
    renderApp("/admin/users");
    await userEvent.click(await screen.findByRole("button", { name: "Add user" }));
    const form = screen.getByRole("form", { name: "Add user" });
    await userEvent.type(within(form).getByLabelText("Email"), "invited@example.com");
    const teams = await within(form).findByRole("group", { name: "Teams" });
    expect(within(teams).queryByLabelText("Role on Payments")).not.toBeInTheDocument();
    await userEvent.click(within(teams).getByRole("checkbox", { name: "Payments" }));
    expect(within(teams).getByLabelText("Role on Payments")).toHaveValue("viewer");
    await userEvent.click(within(teams).getByRole("checkbox", { name: "Platform" }));
    await userEvent.selectOptions(within(teams).getByLabelText("Role on Platform"), "maintainer");
    await userEvent.click(within(form).getByRole("button", { name: "Add" }));
    const [post] = fake.find("POST", "/api/admin/users");
    expect(post.body).toEqual({
      email: "invited@example.com",
      teams: [
        { teamId: 2, role: "viewer" },
        { teamId: 3, role: "maintainer" },
      ],
    });
  });

  it("shows the server's text when the email is added already or is not one, and stays open", async () => {
    let reply = { status: 409, body: { error: "this email is added already" } };
    fakeApi({
      "GET /api/me": { body: operator },
      "GET /api/users": { body: users },
      "GET /api/teams": { body: [] },
      "POST /api/admin/users": () => reply,
    });
    renderApp("/admin/users");
    await userEvent.click(await screen.findByRole("button", { name: "Add user" }));
    const form = screen.getByRole("form", { name: "Add user" });
    await userEvent.type(within(form).getByLabelText("Email"), "dev-user@example.com");
    await userEvent.click(within(form).getByRole("button", { name: "Add" }));
    expect(await within(form).findByRole("alert")).toHaveTextContent("this email is added already");
    reply = { status: 400, body: { error: "that is not an email address" } };
    await userEvent.click(within(form).getByRole("button", { name: "Add" }));
    await vi.waitFor(() => expect(within(form).getByRole("alert")).toHaveTextContent("that is not an email address"));
    expect(screen.getByRole("form", { name: "Add user" })).toBeInTheDocument();
  });

  it("makes an SSO account an admin, and has no choice for a local admin or for yourself", async () => {
    const fake = fakeApi({
      "GET /api/me": { body: operator },
      "GET /api/users": { body: users },
      "PUT /api/admin/users/5/role": { status: 204 },
    });
    renderApp("/admin/users");
    const select = await within(await findRow("dev-user@example.com")).findByLabelText("Role of dev-user@example.com");
    expect(select).toHaveValue("user");
    await userEvent.selectOptions(select, "admin");
    const [put] = fake.find("PUT", "/api/admin/users/5/role");
    expect(put.body).toEqual({ role: "admin" });
    expect(put.headers.get("X-CSRF-Token")).toBe("csrf-123");
    // A local admin is always an admin.
    expect(within(rowOf("admin")).queryByRole("combobox")).not.toBeInTheDocument();
    expect(rowOf("admin")).toHaveTextContent("admin");
  });

  it("gives no role choice for your own SSO account", async () => {
    const self = { ...userDev, role: "admin" as const, username: "sso-admin", email: "sso-admin@example.com" };
    fakeApi({
      "GET /api/me": { body: { ...operator, kind: "sso", username: "sso-admin", email: "sso-admin@example.com" } },
      "GET /api/users": { body: [self, userDev] },
    });
    renderApp("/admin/users");
    const mine = await findRow("sso-admin");
    expect(within(mine).queryByRole("combobox")).not.toBeInTheDocument();
    expect(within(await findRow("dev-user@example.com")).getByRole("combobox")).toBeInTheDocument();
  });

  it("removes an account after a confirmation, with the CSRF token", async () => {
    const confirm = vi.fn(() => true);
    vi.stubGlobal("confirm", confirm);
    let listed = 0;
    const fake = fakeApi({
      "GET /api/me": { body: operator },
      "GET /api/users": () => ({ body: listed++ === 0 ? users : [userDev, userOperator] }),
      "DELETE /api/users/7": { status: 204 },
    });
    renderApp("/admin/users");
    await userEvent.click(within(await findRow("newcomer@example.com")).getByRole("button", { name: "Remove" }));
    expect(confirm).toHaveBeenCalledWith(expect.stringContaining("newcomer@example.com"));
    const [del] = fake.find("DELETE", "/api/users/7");
    expect(del.headers.get("X-CSRF-Token")).toBe("csrf-123");
    await vi.waitFor(() => expect(screen.queryByText("newcomer@example.com")).not.toBeInTheDocument());
    expect(fake.find("GET", "/api/me").length).toBeGreaterThan(1);
  });

  it("removes nothing when the confirmation is declined", async () => {
    vi.stubGlobal("confirm", vi.fn(() => false));
    const fake = fakeApi({ "GET /api/me": { body: operator }, "GET /api/users": { body: users } });
    renderApp("/admin/users");
    await userEvent.click(within(await findRow("newcomer@example.com")).getByRole("button", { name: "Remove" }));
    expect(fake.calls.filter((c) => c.method === "DELETE")).toHaveLength(0);
  });

  it("shows why an account can't be removed", async () => {
    vi.stubGlobal("confirm", vi.fn(() => true));
    fakeApi({
      "GET /api/me": { body: operator },
      "GET /api/users": { body: users },
      "DELETE /api/users/1": { status: 409, body: { error: "the last local admin can't be removed" } },
    });
    renderApp("/admin/users");
    await userEvent.click(within(await findRow("admin")).getByRole("button", { name: "Remove" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("the last local admin can't be removed");
  });

  it("lets an admin add a local admin and shows the generated password once", async () => {
    let listed = 0;
    const fake = fakeApi({
      "GET /api/me": { body: operator },
      "GET /api/users": () => ({ body: listed++ === 0 ? users : [...users, { ...userOperator, id: 9, username: "ops2" }] }),
      "POST /api/admin/accounts": { status: 201, body: { username: "ops2", password: "Zx9-generated-secret-value" } },
    });
    renderApp("/admin/users");
    await userEvent.click(await screen.findByRole("button", { name: "Add local admin" }));
    await userEvent.type(screen.getByLabelText("Username"), "ops2");
    await userEvent.click(screen.getByRole("button", { name: "Create" }));
    const box = await screen.findByLabelText("New local admin");
    expect(box).toHaveTextContent("Zx9-generated-secret-value");
    expect(box).toHaveTextContent("Copy it now: it won't be shown again.");
    // A labelled region, not a live region: a screen reader doesn't read the password out unasked.
    expect(box).not.toHaveAttribute("role", "status");
    expect(box.closest("[aria-live]")).toBeNull();
    expect(within(box).queryByRole("status")).not.toBeInTheDocument();
    expect(screen.getByRole("region", { name: "New local admin" })).toBe(box);
    const [post] = fake.find("POST", "/api/admin/accounts");
    expect(post.body).toEqual({ username: "ops2" });
    expect(post.headers.get("X-CSRF-Token")).toBe("csrf-123");
    // The list shows the new account behind the box.
    expect(await screen.findByText("ops2")).toBeInTheDocument();

    await userEvent.click(screen.getByRole("button", { name: "Done" }));
    expect(screen.queryByText("Zx9-generated-secret-value")).not.toBeInTheDocument();
    expect(screen.queryByLabelText("New local admin")).not.toBeInTheDocument();
    expect(document.body.textContent).not.toContain("Zx9-generated-secret-value");
  });

  it("shows the server's text when the username is taken", async () => {
    fakeApi({
      "GET /api/me": { body: operator },
      "GET /api/users": { body: users },
      "POST /api/admin/accounts": { status: 409, body: { error: "this username is taken" } },
    });
    renderApp("/admin/users");
    await userEvent.click(await screen.findByRole("button", { name: "Add local admin" }));
    await userEvent.type(screen.getByLabelText("Username"), "admin");
    await userEvent.click(screen.getByRole("button", { name: "Create" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("this username is taken");
  });
});
