import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import type { TeamPageView, UserView } from "../api";
import { fakeApi } from "../test/fakeApi";
import { maintainer, myTeams, ssoAdmin, teamModelsPayments, teamPagePayments, chainTeam } from "../test/fixtures";
import { renderApp } from "../test/render";

afterEach(() => vi.unstubAllGlobals());

const asAdmin: TeamPageView = { ...teamPagePayments, role: "admin", canEdit: true };

const account = (id: number, username: string, kind: UserView["kind"] = "sso"): UserView => ({
  id,
  kind,
  username,
  email: `${username}@example.com`,
  role: "user",
  invited: false,
  lastSignInAt: null,
  waiting: false,
  teams: [],
});
const accounts: UserView[] = [
  account(11, "ana"),
  account(12, "bo"),
  account(20, "dev-one"),
  account(21, "Dev-Two"),
  account(22, "carol"),
  account(23, "root", "password"),
];

const routes = (me = ssoAdmin, page: TeamPageView = asAdmin) => ({
  "GET /api/me": { body: me },
  "GET /api/me/teams": { body: myTeams },
  "GET /api/teams/2": { body: page },
  "GET /api/teams/2/models": { body: teamModelsPayments },
  "GET /api/teams/2/chain": { body: chainTeam },
  "GET /api/users": { body: accounts },
  "PUT /api/teams/2": { body: {} },
});

const reposAsRead = [
  { instance: "main", project: "acme/payments", owner: true },
  { instance: "side", project: "", owner: false },
];

describe("Team Members tab", () => {
  it("a member sees usernames and roles, no controls, and no account list is read", async () => {
    const fake = fakeApi(routes(maintainer, teamPagePayments));
    renderApp("/teams/2?tab=members");
    const list = await screen.findByRole("region", { name: "Team members" });
    expect(within(list).getByText("ana")).toBeInTheDocument();
    expect(within(list).getByText("maintainer")).toBeInTheDocument();
    expect(within(list).getByText("bo")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Add member" })).not.toBeInTheDocument();
    expect(screen.queryByRole("combobox")).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /^Remove / })).not.toBeInTheDocument();
    expect(fake.calls.filter((c) => c.url === "/api/users")).toHaveLength(0);
  });

  it("Add member lists nothing until typed, then the matching SSO accounts not on the team, at most 10", async () => {
    const many = Array.from({ length: 12 }, (_, i) => account(100 + i, `dev-many-${i}`));
    const fake = fakeApi({ ...routes(), "GET /api/users": { body: [...accounts, ...many] } });
    renderApp("/teams/2?tab=members");
    await userEvent.click(await screen.findByRole("button", { name: "Add member" }));
    const picker = screen.getByRole("region", { name: "Add member" });
    expect(within(picker).queryByRole("button", { name: /^Add dev/ })).not.toBeInTheDocument();
    await userEvent.type(within(picker).getByRole("searchbox", { name: "Search" }), "DEV");
    expect(await within(picker).findByRole("button", { name: "Add dev-one" })).toBeInTheDocument();
    expect(within(picker).getByRole("button", { name: "Add Dev-Two" })).toBeInTheDocument();
    expect(within(picker).getAllByRole("button", { name: /^Add / })).toHaveLength(10);
    // ana is on the team, root is a password account, carol does not match.
    await userEvent.clear(within(picker).getByRole("searchbox", { name: "Search" }));
    await userEvent.type(within(picker).getByRole("searchbox", { name: "Search" }), "a");
    expect(within(picker).queryByRole("button", { name: "Add ana" })).not.toBeInTheDocument();
    expect(within(picker).queryByRole("button", { name: "Add root" })).not.toBeInTheDocument();
    expect(within(picker).getByRole("button", { name: "Add carol" })).toBeInTheDocument();
    expect(fake.find("GET", "/api/users")).toHaveLength(1);
  });

  it("Add PUTs the new account as viewer, with the repositories as read", async () => {
    const fake = fakeApi(routes());
    renderApp("/teams/2?tab=members");
    await userEvent.click(await screen.findByRole("button", { name: "Add member" }));
    const picker = screen.getByRole("region", { name: "Add member" });
    await userEvent.type(within(picker).getByRole("searchbox", { name: "Search" }), "carol");
    await userEvent.click(await within(picker).findByRole("button", { name: "Add carol" }));
    await vi.waitFor(() => expect(fake.find("PUT", "/api/teams/2")).toHaveLength(1));
    expect(fake.find("PUT", "/api/teams/2")[0].body).toEqual({
      name: "Payments",
      members: [
        { accountId: 11, role: "maintainer" },
        { accountId: 12, role: "viewer" },
        { accountId: 22, role: "viewer" },
      ],
      repos: reposAsRead,
    });
  });

  it("a role change and Remove each PUT one change, repositories as read", async () => {
    const fake = fakeApi(routes());
    renderApp("/teams/2?tab=members");
    const list = await screen.findByRole("region", { name: "Team members" });
    await userEvent.selectOptions(within(list).getByRole("combobox", { name: "Role of bo" }), "maintainer");
    await vi.waitFor(() => expect(fake.find("PUT", "/api/teams/2")).toHaveLength(1));
    expect(fake.find("PUT", "/api/teams/2")[0].body).toEqual({
      name: "Payments",
      members: [
        { accountId: 11, role: "maintainer" },
        { accountId: 12, role: "maintainer" },
      ],
      repos: reposAsRead,
    });
    await userEvent.click(within(list).getByRole("button", { name: "Remove ana" }));
    await vi.waitFor(() => expect(fake.find("PUT", "/api/teams/2")).toHaveLength(2));
    expect(fake.find("PUT", "/api/teams/2")[1].body).toEqual({
      name: "Payments",
      members: [{ accountId: 12, role: "viewer" }],
      repos: reposAsRead,
    });
  });

  it("Add by email POSTs the email and the team, and makes no PUT", async () => {
    const fake = fakeApi({ ...routes(), "POST /api/admin/users": { status: 201, body: account(30, "new") } });
    renderApp("/teams/2?tab=members");
    await userEvent.type(await screen.findByLabelText("Add by email"), "new@example.com");
    await userEvent.click(screen.getByRole("button", { name: "Add email" }));
    await vi.waitFor(() => expect(fake.find("POST", "/api/admin/users")).toHaveLength(1));
    expect(fake.find("POST", "/api/admin/users")[0].body).toEqual({
      email: "new@example.com",
      teams: [{ teamId: 2, role: "viewer" }],
    });
    await vi.waitFor(() => expect(screen.getByLabelText("Add by email")).toHaveValue(""));
    expect(fake.find("PUT", "/api/teams/2")).toHaveLength(0);
  });

  it("on a 409 it reads the accounts and PUTs the team with that account as viewer", async () => {
    const fake = fakeApi({
      ...routes(),
      "POST /api/admin/users": { status: 409, body: { error: "this email is added already" } },
    });
    renderApp("/teams/2?tab=members");
    await userEvent.type(await screen.findByLabelText("Add by email"), "Carol@Example.com");
    await userEvent.click(screen.getByRole("button", { name: "Add email" }));
    await vi.waitFor(() => expect(fake.find("PUT", "/api/teams/2")).toHaveLength(1));
    expect(fake.find("GET", "/api/users").length).toBeGreaterThan(0);
    expect(fake.find("PUT", "/api/teams/2")[0].body).toEqual({
      name: "Payments",
      members: [
        { accountId: 11, role: "maintainer" },
        { accountId: 12, role: "viewer" },
        { accountId: 22, role: "viewer" },
      ],
      repos: reposAsRead,
    });
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  });

  it("on a 409 for an account that is not found, it keeps the text, shows an error and makes no PUT", async () => {
    const fake = fakeApi({
      ...routes(),
      "POST /api/admin/users": { status: 409, body: { error: "this email is added already" } },
    });
    renderApp("/teams/2?tab=members");
    await userEvent.type(await screen.findByLabelText("Add by email"), "ghost@example.com");
    await userEvent.click(screen.getByRole("button", { name: "Add email" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("This email is added already, but its account was not found.");
    expect(screen.getByLabelText("Add by email")).toHaveValue("ghost@example.com");
    expect(fake.find("PUT", "/api/teams/2")).toHaveLength(0);
  });

  it("on a 409 for an account on the team already, it says so and makes no PUT", async () => {
    const fake = fakeApi({
      ...routes(),
      "POST /api/admin/users": { status: 409, body: { error: "this email is added already" } },
    });
    renderApp("/teams/2?tab=members");
    await userEvent.type(await screen.findByLabelText("Add by email"), "ana@example.com");
    await userEvent.click(screen.getByRole("button", { name: "Add email" }));
    expect(await screen.findByText("Already on this team.")).toBeInTheDocument();
    expect(fake.find("PUT", "/api/teams/2")).toHaveLength(0);
  });
});

