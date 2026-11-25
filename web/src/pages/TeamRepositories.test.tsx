import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import type { TeamPageView } from "../api";
import { fakeApi } from "../test/fakeApi";
import { maintainer, myTeams, ssoAdmin, teamChoicesPay, teamChoicesPay2, teamModelsPayments, teamPagePayments, chainTeam } from "../test/fixtures";
import { renderApp } from "../test/render";

// Remove saves at once, so it asks first: the tests answer yes unless one
// says otherwise.
beforeEach(() => {
  vi.spyOn(window, "confirm").mockReturnValue(true);
});
afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

const asAdmin: TeamPageView = { ...teamPagePayments, role: "admin", canEdit: true };
const choices = "/api/teams/2/repository-choices";

const routes = (me = ssoAdmin, page: TeamPageView = asAdmin) => ({
  "GET /api/me": { body: me },
  "GET /api/me/teams": { body: myTeams },
  "GET /api/teams/2": { body: page },
  "GET /api/teams/2/models": { body: teamModelsPayments },
  "GET /api/teams/2/chain": { body: chainTeam },
  "PUT /api/teams/2": { body: {} },
});

const membersAsRead = [
  { accountId: 11, role: "maintainer" },
  { accountId: 12, role: "viewer" },
];
const reposAsRead = [
  { instance: "main", project: "acme/payments", owner: true },
  { instance: "side", project: "", owner: false },
];

describe("Team Repositories tab", () => {
  it("lists the team's entries", async () => {
    fakeApi(routes(maintainer, teamPagePayments));
    renderApp("/teams/2");
    const list = await screen.findByRole("region", { name: "Team repositories" });
    expect(within(list).getByRole("link", { name: "acme/payments" })).toHaveAttribute("href", "/repos/main/acme/payments");
    expect(within(list).getByText("owner")).toBeInTheDocument();
    expect(within(list).getByText("All repositories of side")).toBeInTheDocument();
    expect(within(list).getByRole("link", { name: "acme/old" })).toHaveAttribute("href", "/repos/main/acme/old");
    expect(within(list).getByText("(removed)")).toBeInTheDocument();
    expect(within(list).getByText("The next save drops it.")).toBeInTheDocument();
  });

  it("shows paused as a link to the repository", async () => {
    const page: TeamPageView = {
      ...teamPagePayments,
      repos: [{ instance: "main", project: "acme/payments", owner: false, paused: true, removed: false }],
    };
    fakeApi(routes(maintainer, page));
    renderApp("/teams/2");
    expect(await screen.findByRole("link", { name: "paused" })).toHaveAttribute("href", "/repos/main/acme/payments");
  });

  it("a member sees no Add repository, owner toggle or Remove", async () => {
    fakeApi(routes(maintainer, teamPagePayments));
    renderApp("/teams/2");
    await screen.findByRole("region", { name: "Team repositories" });
    expect(screen.queryByRole("button", { name: "Add repository" })).not.toBeInTheDocument();
    expect(screen.queryByRole("checkbox")).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /^Remove / })).not.toBeInTheDocument();
  });

  it("Add repository shows no list until searched, and Load more asks for page 2", async () => {
    const fake = fakeApi({
      ...routes(),
      [`GET ${choices}?search=pay&page=1`]: { body: teamChoicesPay },
      [`GET ${choices}?search=pay&page=2`]: { body: teamChoicesPay2 },
    });
    renderApp("/teams/2");
    await userEvent.click(await screen.findByRole("button", { name: "Add repository" }));
    const picker = screen.getByRole("region", { name: "Add repository" });
    expect(fake.calls.filter((c) => c.url.includes("repository-choices"))).toHaveLength(0);
    await userEvent.type(within(picker).getByRole("searchbox", { name: "Search" }), "pay");
    await userEvent.click(within(picker).getByRole("button", { name: "Search" }));
    expect(await within(picker).findByText("acme/payments-api")).toBeInTheDocument();
    expect(fake.find("GET", `${choices}?search=pay&page=1`)).toHaveLength(1);
    await userEvent.click(within(picker).getByRole("button", { name: "Load more" }));
    expect(await within(picker).findByText("acme/paybox")).toBeInTheDocument();
    expect(fake.find("GET", `${choices}?search=pay&page=2`)).toHaveLength(1);
    expect(within(picker).queryByRole("button", { name: "Load more" })).not.toBeInTheDocument();
  });

  it("another team's owner disables the owner box", async () => {
    fakeApi({ ...routes(), [`GET ${choices}?search=pay&page=1`]: { body: teamChoicesPay } });
    renderApp("/teams/2");
    await userEvent.click(await screen.findByRole("button", { name: "Add repository" }));
    const picker = screen.getByRole("region", { name: "Add repository" });
    await userEvent.type(within(picker).getByRole("searchbox", { name: "Search" }), "pay");
    await userEvent.click(within(picker).getByRole("button", { name: "Search" }));
    expect(await within(picker).findByText("owned by Widgets")).toBeInTheDocument();
    expect(within(picker).getByRole("checkbox", { name: "Owner of acme/payroll" })).toBeDisabled();
    expect(within(picker).getByRole("checkbox", { name: "Owner of acme/payments-api" })).toBeEnabled();
  });

  it("adding sends the members as read and the old entries plus the new one", async () => {
    const fake = fakeApi({ ...routes(), [`GET ${choices}?search=pay&page=1`]: { body: teamChoicesPay } });
    renderApp("/teams/2");
    await userEvent.click(await screen.findByRole("button", { name: "Add repository" }));
    const picker = screen.getByRole("region", { name: "Add repository" });
    await userEvent.type(within(picker).getByRole("searchbox", { name: "Search" }), "pay");
    await userEvent.click(within(picker).getByRole("button", { name: "Search" }));
    await userEvent.click(await within(picker).findByRole("checkbox", { name: "Owner of acme/payments-api" }));
    await userEvent.click(within(picker).getByRole("button", { name: "Add acme/payments-api" }));
    const [put] = fake.find("PUT", "/api/teams/2");
    expect(put.body).toEqual({
      name: "Payments",
      members: membersAsRead,
      repos: [...reposAsRead, { instance: "main", project: "acme/payments-api", owner: true }],
    });
  });

  it("owner toggle and Remove each PUT one change", async () => {
    const fake = fakeApi(routes());
    renderApp("/teams/2");
    await userEvent.click(await screen.findByRole("checkbox", { name: "Owner of All repositories of side" }));
    await vi.waitFor(() => expect(fake.find("PUT", "/api/teams/2")).toHaveLength(1));
    expect(fake.find("PUT", "/api/teams/2")[0].body).toEqual({
      name: "Payments",
      members: membersAsRead,
      repos: [
        { instance: "main", project: "acme/payments", owner: true },
        { instance: "side", project: "", owner: true },
      ],
    });
    await userEvent.click(screen.getByRole("button", { name: "Remove acme/payments" }));
    await vi.waitFor(() => expect(fake.find("PUT", "/api/teams/2")).toHaveLength(2));
    expect(fake.find("PUT", "/api/teams/2")[1].body).toEqual({
      name: "Payments",
      members: membersAsRead,
      repos: [{ instance: "side", project: "", owner: false }],
    });
  });

  it("a refused save shows the server's text in an alert", async () => {
    fakeApi({ ...routes(), "PUT /api/teams/2": { status: 409, body: { error: "acme/widgets already has owner Widgets" } } });
    renderApp("/teams/2");
    await userEvent.click(await screen.findByRole("button", { name: "Remove acme/payments" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("acme/widgets already has owner Widgets");
  });

  it("a saved change reads the chain views again", async () => {
    fakeApi(routes());
    const { client } = renderApp("/teams/2");
    await screen.findByRole("region", { name: "Team repositories" });
    const spy = vi.spyOn(client, "invalidateQueries");
    await userEvent.click(screen.getByRole("button", { name: "Remove acme/payments" }));
    const keys = () => spy.mock.calls.map((c) => JSON.stringify((c[0] as { queryKey: unknown[] }).queryKey));
    await vi.waitFor(() => expect(keys()).toContain('["repo-chain"]'));
    for (const k of ['["team-models",2]', '["team-chain",2]', '["team-limits",2]', '["team-repository-choices",2]', '["repo-chain"]', '["repo-reviewers"]']) {
      expect(keys()).toContain(k);
    }
  });
});

describe("TeamRepositories: Remove asks first", () => {
  it("removes nothing when the question is answered no", async () => {
    vi.mocked(window.confirm).mockReturnValue(false);
    const fake = fakeApi(routes());
    renderApp("/teams/2");
    await userEvent.click(await screen.findByRole("button", { name: "Remove acme/payments" }));
    expect(window.confirm).toHaveBeenCalledWith(
      "Remove acme/payments from this team? Its members lose the access it gives, and it stops using this team's models.",
    );
    expect(fake.find("PUT", "/api/teams/2")).toHaveLength(0);
  });
});
