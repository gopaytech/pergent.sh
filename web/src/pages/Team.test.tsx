import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import type { TeamPageView } from "../api";
import { fakeApi } from "../test/fakeApi";
import { chainTeam, maintainer, myTeams, ssoAdmin, teamLimitsPayments, teamModelsPayments, teamPagePayments, viewer } from "../test/fixtures";
import { renderApp } from "../test/render";

afterEach(() => vi.unstubAllGlobals());

const asViewer: TeamPageView = { ...teamPagePayments, role: "viewer" };
const asAdmin: TeamPageView = { ...teamPagePayments, role: "admin", canEdit: true };

const routes = (me = maintainer, page: TeamPageView = teamPagePayments) => ({
  "GET /api/me": { body: me },
  "GET /api/me/teams": { body: myTeams },
  "GET /api/teams/2": { body: page },
  "GET /api/teams/2/models": { body: teamModelsPayments },
  "GET /api/teams/2/chain": { body: chainTeam },
  "GET /api/teams/2/limits": { body: teamLimitsPayments },
});

const tabNames = async () => (await screen.findAllByRole("tab")).map((t) => t.textContent);

describe("Team page", () => {
  it("links a maintainer to the picker with the team, when an instance lets teams onboard", async () => {
    fakeApi(routes());
    const first = renderApp("/teams/2");
    const add = await screen.findByRole("link", { name: "+ Add repository" });
    expect(add).toHaveAttribute("href", "/repos?add=1&team=2");
    first.unmount();
    fakeApi(routes({ ...maintainer, onboardInstances: [] }));
    renderApp("/teams/2");
    await screen.findByRole("heading", { name: "Payments" });
    expect(screen.queryByRole("link", { name: "+ Add repository" })).not.toBeInTheDocument();
  });

  it("gives a viewer of the team no + Add repository", async () => {
    fakeApi(routes({ ...viewer, onboardInstances: ["main"] }, asViewer));
    renderApp("/teams/2");
    await screen.findByRole("heading", { name: "Payments" });
    expect(screen.queryByRole("link", { name: "+ Add repository" })).not.toBeInTheDocument();
  });

  it("viewer sees Repositories, Members, Models only", async () => {
    // GET /api/teams/2/usage is not in routes: fakeApi would answer 404, and the calls list shows it.
    const fake = fakeApi(routes(viewer, asViewer));
    renderApp("/teams/2");
    expect(await screen.findByRole("heading", { name: "Payments" })).toBeInTheDocument();
    expect(await tabNames()).toEqual(["Repositories", "Members", "Models"]);
    expect(screen.getByText("Your role: viewer")).toBeInTheDocument();
    expect(fake.calls.filter((c) => c.url.includes("/usage"))).toHaveLength(0);
  });

  it("maintainer and admin see Limits and Usage", async () => {
    fakeApi(routes());
    const { unmount } = renderApp("/teams/2");
    expect(await tabNames()).toEqual(["Repositories", "Members", "Models", "Limits", "Usage"]);
    unmount();
    fakeApi(routes(ssoAdmin, asAdmin));
    renderApp("/teams/2");
    expect(await tabNames()).toEqual(["Repositories", "Members", "Models", "Limits", "Usage"]);
  });

  it("shows the tab named in the address, and Limits for a maintainer", async () => {
    fakeApi(routes());
    renderApp("/teams/2?tab=limits");
    expect(await screen.findByRole("region", { name: "Model limits" })).toBeInTheDocument();
    expect(screen.getByRole("tab", { name: "Limits" })).toHaveAttribute("aria-selected", "true");
  });

  it("puts the chosen tab in the address, and none for Repositories", async () => {
    fakeApi(routes());
    renderApp("/teams/2");
    await userEvent.click(await screen.findByRole("tab", { name: "Models" }));
    expect(screen.getByTestId("location")).toHaveTextContent("/teams/2?tab=models");
    expect(await screen.findByRole("region", { name: "Team chain" })).toBeInTheDocument();
    await userEvent.click(screen.getByRole("tab", { name: "Repositories" }));
    expect(screen.getByTestId("location")).toHaveTextContent(/^\/teams\/2$/);
  });

  it("?tab=usage as a viewer shows Repositories", async () => {
    fakeApi(routes(viewer, asViewer));
    renderApp("/teams/2?tab=usage");
    expect(await screen.findByRole("heading", { name: "Payments" })).toBeInTheDocument();
    expect(screen.getByRole("tab", { name: "Repositories" })).toHaveAttribute("aria-selected", "true");
  });

  it("admin sees Rename and Remove team, a maintainer does not", async () => {
    fakeApi(routes());
    const { unmount } = renderApp("/teams/2");
    expect(await screen.findByRole("heading", { name: "Payments" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Rename" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Remove team" })).not.toBeInTheDocument();
    unmount();
    fakeApi(routes(ssoAdmin, asAdmin));
    renderApp("/teams/2");
    expect(await screen.findByRole("button", { name: "Rename" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Remove team" })).toBeInTheDocument();
  });

  it("Rename sends the new name with members and repositories as read", async () => {
    const fake = fakeApi({ ...routes(ssoAdmin, asAdmin), "PUT /api/teams/2": { body: {} } });
    renderApp("/teams/2");
    await userEvent.click(await screen.findByRole("button", { name: "Rename" }));
    const form = screen.getByRole("form", { name: "Rename the team" });
    await userEvent.clear(within(form).getByLabelText("Name"));
    await userEvent.type(within(form).getByLabelText("Name"), "Billing");
    await userEvent.click(within(form).getByRole("button", { name: "Save" }));
    const [put] = fake.find("PUT", "/api/teams/2");
    expect(put.headers.get("X-CSRF-Token")).toBe("csrf-123");
    expect(put.body).toEqual({
      name: "Billing",
      members: [
        { accountId: 11, role: "maintainer" },
        { accountId: 12, role: "viewer" },
      ],
      repos: [
        { instance: "main", project: "acme/payments", owner: true },
        { instance: "side", project: "", owner: false },
      ],
    });
  });

  it("Remove team asks, deletes, and lands on the admin Teams page", async () => {
    const confirm = vi.fn(() => true);
    vi.stubGlobal("confirm", confirm);
    const fake = fakeApi({ ...routes(ssoAdmin, asAdmin), "DELETE /api/teams/2": { status: 204 }, "GET /api/teams": { body: [] }, "GET /api/users": { body: [] } });
    renderApp("/teams/2");
    await userEvent.click(await screen.findByRole("button", { name: "Remove team" }));
    expect(confirm).toHaveBeenCalledWith("Remove the team Payments? Its members stay, but lose the access it gave them.");
    expect(fake.find("DELETE", "/api/teams/2")).toHaveLength(1);
    await vi.waitFor(() => expect(screen.getByTestId("location")).toHaveTextContent(/^\/admin\/teams$/));
  });

  it("removes nothing when the confirmation is refused", async () => {
    vi.stubGlobal("confirm", vi.fn(() => false));
    const fake = fakeApi(routes(ssoAdmin, asAdmin));
    renderApp("/teams/2");
    await userEvent.click(await screen.findByRole("button", { name: "Remove team" }));
    expect(fake.find("DELETE", "/api/teams/2")).toHaveLength(0);
  });

  it("a non-member gets Not found when the route answers 404", async () => {
    fakeApi({ "GET /api/me": { body: viewer }, "GET /api/teams/9": { status: 404, body: { error: "not found" } } });
    renderApp("/teams/9");
    expect(await screen.findByRole("heading", { name: "Not found" })).toBeInTheDocument();
  });

  it("is Not found for an id that is not a number", async () => {
    fakeApi({ "GET /api/me": { body: viewer } });
    renderApp("/teams/abc");
    expect(await screen.findByRole("heading", { name: "Not found" })).toBeInTheDocument();
  });
});
