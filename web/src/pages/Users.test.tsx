import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { fakeApi } from "../test/fakeApi";
import { operator, platformAdmin, userDev, userOperator, userWaiting } from "../test/fixtures";
import { renderApp } from "../test/render";

afterEach(() => vi.unstubAllGlobals());

// The API sends the waiting accounts first; the page keeps them there.
const users = [userDev, userOperator, userWaiting];

const rowOf = (name: string) => screen.getByText(name).closest("tr") as HTMLElement;
const findRow = async (name: string) => (await screen.findByText(name)).closest("tr") as HTMLElement;

describe("Users", () => {
  it("lists the waiting accounts first, marked waiting", async () => {
    fakeApi({ "GET /api/me": { body: operator }, "GET /api/users": { body: users } });
    renderApp("/users");
    expect(await screen.findByRole("heading", { name: "Users" })).toBeInTheDocument();
    await screen.findByText("newcomer");
    const rows = screen.getAllByRole("row").slice(1);
    expect(rows.map((r) => within(r).getAllByRole("cell")[0].textContent)).toEqual([
      "newcomerwaiting",
      "dev-user",
      "admin",
    ]);
    expect(within(rowOf("newcomer")).getByText("waiting")).toHaveClass("pill", "warn");
    expect(within(rowOf("dev-user")).queryByText("waiting")).not.toBeInTheDocument();
    expect(rowOf("dev-user")).toHaveTextContent("Payments");
    expect(rowOf("dev-user")).toHaveTextContent("GitLab · main");
    expect(rowOf("admin")).toHaveTextContent("Password admin");
    expect(rowOf("admin")).toHaveTextContent("never");
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
    renderApp("/users");
    await userEvent.click(within(await findRow("newcomer")).getByRole("button", { name: "Remove" }));
    expect(confirm).toHaveBeenCalledWith(expect.stringContaining("newcomer"));
    const [del] = fake.find("DELETE", "/api/users/7");
    expect(del.headers.get("X-CSRF-Token")).toBe("csrf-123");
    await vi.waitFor(() => expect(screen.queryByText("newcomer")).not.toBeInTheDocument());
    expect(fake.find("GET", "/api/me").length).toBeGreaterThan(1);
  });

  it("removes nothing when the confirmation is declined", async () => {
    vi.stubGlobal("confirm", vi.fn(() => false));
    const fake = fakeApi({ "GET /api/me": { body: operator }, "GET /api/users": { body: users } });
    renderApp("/users");
    await userEvent.click(within(await findRow("newcomer")).getByRole("button", { name: "Remove" }));
    expect(fake.calls.filter((c) => c.method === "DELETE")).toHaveLength(0);
  });

  it("shows why an account can't be removed", async () => {
    vi.stubGlobal("confirm", vi.fn(() => true));
    fakeApi({
      "GET /api/me": { body: operator },
      "GET /api/users": { body: users },
      "DELETE /api/users/1": { status: 409, body: { error: "the last password account can't be removed" } },
    });
    renderApp("/users");
    await userEvent.click(within(await findRow("admin")).getByRole("button", { name: "Remove" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("the last password account can't be removed");
  });

  it("lets an operator add a password admin and shows the generated password once", async () => {
    let listed = 0;
    const fake = fakeApi({
      "GET /api/me": { body: operator },
      "GET /api/users": () => ({ body: listed++ === 0 ? users : [...users, { ...userOperator, id: 9, username: "ops2" }] }),
      "POST /api/admin/accounts": { status: 201, body: { username: "ops2", password: "Zx9-generated-secret-value" } },
    });
    renderApp("/users");
    await userEvent.click(await screen.findByRole("button", { name: "Add password admin" }));
    await userEvent.type(screen.getByLabelText("Username"), "ops2");
    await userEvent.click(screen.getByRole("button", { name: "Create" }));
    const box = await screen.findByLabelText("New password admin");
    expect(box).toHaveTextContent("Zx9-generated-secret-value");
    expect(box).toHaveTextContent("Copy it now: it won't be shown again.");
    // A labelled region, not a live region: a screen reader doesn't read the password out unasked.
    expect(box).not.toHaveAttribute("role", "status");
    expect(box.closest("[aria-live]")).toBeNull();
    expect(within(box).queryByRole("status")).not.toBeInTheDocument();
    expect(screen.getByRole("region", { name: "New password admin" })).toBe(box);
    const [post] = fake.find("POST", "/api/admin/accounts");
    expect(post.body).toEqual({ username: "ops2" });
    expect(post.headers.get("X-CSRF-Token")).toBe("csrf-123");
    // The list shows the new account behind the box.
    expect(await screen.findByText("ops2")).toBeInTheDocument();

    await userEvent.click(screen.getByRole("button", { name: "Done" }));
    expect(screen.queryByText("Zx9-generated-secret-value")).not.toBeInTheDocument();
    expect(screen.queryByLabelText("New password admin")).not.toBeInTheDocument();
    expect(document.body.textContent).not.toContain("Zx9-generated-secret-value");
  });

  it("shows the server's text when the username is taken", async () => {
    fakeApi({
      "GET /api/me": { body: operator },
      "GET /api/users": { body: users },
      "POST /api/admin/accounts": { status: 409, body: { error: "this username is taken" } },
    });
    renderApp("/users");
    await userEvent.click(await screen.findByRole("button", { name: "Add password admin" }));
    await userEvent.type(screen.getByLabelText("Username"), "admin");
    await userEvent.click(screen.getByRole("button", { name: "Create" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("this username is taken");
  });

  it("gives a platform admin no way to add or remove a password account", async () => {
    fakeApi({ "GET /api/me": { body: platformAdmin }, "GET /api/users": { body: users } });
    renderApp("/users");
    await screen.findByText("newcomer");
    expect(screen.queryByRole("button", { name: "Add password admin" })).not.toBeInTheDocument();
    expect(within(rowOf("admin")).queryByRole("button", { name: "Remove" })).not.toBeInTheDocument();
    expect(within(rowOf("dev-user")).getByRole("button", { name: "Remove" })).toBeInTheDocument();
  });
});
