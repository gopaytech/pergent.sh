import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import type { TeamLimitsView, TeamModelsView } from "../api";
import { fakeApi } from "../test/fakeApi";
import { chainTeam, maintainer, myTeams, ssoAdmin, teamLimitsPayments, teamModelQwen, teamModelsPayments, teamPagePayments, viewer } from "../test/fixtures";
import { renderApp } from "../test/render";

afterEach(() => vi.unstubAllGlobals());

const routes = (me = maintainer, models: TeamModelsView = teamModelsPayments, limits: TeamLimitsView = teamLimitsPayments) => ({
  "GET /api/me": { body: me },
  "GET /api/me/teams": { body: myTeams },
  "GET /api/teams/2": { body: { ...teamPagePayments, role: models.role } },
  "GET /api/teams/2/models": { body: models },
  "GET /api/teams/2/chain": { body: chainTeam },
  "GET /api/teams/2/limits": { body: limits },
});

const card = () => screen.findByRole("region", { name: "Model limits" });

describe("TeamModels: model limits", () => {
  it("shows a maintainer the team's own limits to set, and an admin's limit read-only", async () => {
    fakeApi(routes());
    renderApp("/teams/2?tab=limits");
    const c = await card();
    const own = await within(c).findByRole("group", { name: "qwen3.5 limit" });
    expect(own).toHaveTextContent("Tokens this month: 450 of 500");
    expect(within(own).getByRole("meter", { name: "Tokens of qwen3.5 this month" })).toHaveClass("warn");
    expect(within(own).getByLabelText("Limit of qwen3.5")).toHaveValue("500");
    const sonnet = within(c).getByRole("group", { name: "Claude Sonnet limit" });
    expect(sonnet).toHaveTextContent("Tokens this month: 150 of 1,000");
    expect(sonnet).toHaveTextContent("Set by an admin.");
    expect(sonnet).toHaveTextContent("Model limit for all teams: used up");
    expect(within(sonnet).queryByRole("textbox")).not.toBeInTheDocument();
    expect(within(sonnet).queryByRole("button")).not.toBeInTheDocument();
  });

  it("sets and removes the limit of the team's own model with the team's route", async () => {
    const fake = fakeApi({
      ...routes(),
      "PUT /api/teams/2/models/t-qwen/limit": { status: 204 },
      "DELETE /api/teams/2/models/t-qwen/limit": { status: 204 },
    });
    renderApp("/teams/2?tab=limits");
    const own = await within(await card()).findByRole("group", { name: "qwen3.5 limit" });
    const input = within(own).getByLabelText("Limit of qwen3.5");
    const save = within(own).getByRole("button", { name: "Save the limit of qwen3.5" });
    await userEvent.clear(input);
    await userEvent.type(input, "800");
    await userEvent.click(save);
    await waitFor(() => expect(fake.find("PUT", "/api/teams/2/models/t-qwen/limit")).toHaveLength(1));
    const [put] = fake.find("PUT", "/api/teams/2/models/t-qwen/limit");
    expect(put.body).toEqual({ monthlyTokens: 800 });
    expect(put.headers.get("X-CSRF-Token")).toBe("csrf-maint");
    // The card is read again. The team chain, whose tags move, is on the Models tab: its cache is marked stale and read when that tab opens.
    await waitFor(() => expect(fake.find("GET", "/api/teams/2/limits").length).toBeGreaterThanOrEqual(2));
    await userEvent.clear(input);
    await userEvent.click(save);
    await waitFor(() => expect(fake.find("DELETE", "/api/teams/2/models/t-qwen/limit")).toHaveLength(1));
  });

  it("drops the server's message when a later value is refused before any request", async () => {
    fakeApi({
      ...routes(),
      "PUT /api/teams/2/models/t-qwen/limit": { status: 400, body: { error: "the server says no" } },
    });
    renderApp("/teams/2?tab=limits");
    const own = await within(await card()).findByRole("group", { name: "qwen3.5 limit" });
    const input = within(own).getByLabelText("Limit of qwen3.5");
    const save = within(own).getByRole("button", { name: "Save the limit of qwen3.5" });
    await userEvent.clear(input);
    await userEvent.type(input, "800");
    await userEvent.click(save);
    expect(await within(own).findByText("the server says no")).toBeInTheDocument();
    await userEvent.clear(input);
    await userEvent.type(input, "abc");
    await userEvent.click(save);
    expect(await within(own).findByText(/whole number from 1 to/)).toBeInTheDocument();
    expect(within(own).queryByText("the server says no")).not.toBeInTheDocument();
  });

  it("lets an admin change the team's limit on a global model with the admin route", async () => {
    const fake = fakeApi({
      ...routes(ssoAdmin, { ...teamModelsPayments, role: "admin" }),
      "PUT /api/admin/models/m-sonnet/team-limits/2": { status: 204 },
    });
    renderApp("/teams/2?tab=limits");
    const sonnet = await within(await card()).findByRole("group", { name: "Claude Sonnet limit" });
    expect(sonnet).not.toHaveTextContent("Set by an admin.");
    const input = within(sonnet).getByLabelText("Limit of Claude Sonnet");
    expect(input).toHaveValue("1000");
    await userEvent.clear(input);
    await userEvent.type(input, "2000");
    await userEvent.click(within(sonnet).getByRole("button", { name: "Save the limit of Claude Sonnet" }));
    await waitFor(() => expect(fake.find("PUT", "/api/admin/models/m-sonnet/team-limits/2")).toHaveLength(1));
    expect(fake.find("PUT", "/api/admin/models/m-sonnet/team-limits/2")[0].body).toEqual({ monthlyTokens: 2000 });
  });

  it("lets an admin remove the team's limit on a global model with the admin route", async () => {
    const fake = fakeApi({
      ...routes(ssoAdmin, { ...teamModelsPayments, role: "admin" }),
      "DELETE /api/admin/models/m-sonnet/team-limits/2": { status: 204 },
    });
    renderApp("/teams/2?tab=limits");
    const sonnet = await within(await card()).findByRole("group", { name: "Claude Sonnet limit" });
    await userEvent.clear(within(sonnet).getByLabelText("Limit of Claude Sonnet"));
    await userEvent.click(within(sonnet).getByRole("button", { name: "Save the limit of Claude Sonnet" }));
    await waitFor(() => expect(fake.find("DELETE", "/api/admin/models/m-sonnet/team-limits/2")).toHaveLength(1));
    expect(fake.find("DELETE", "/api/teams/2/models/m-sonnet/limit")).toHaveLength(0);
  });

  it("says only whether a global model's own limit has room, and nothing when it has none", async () => {
    const limits: TeamLimitsView = {
      ...teamLimitsPayments,
      global: [
        { ...teamLimitsPayments.global[0], modelLimit: "room_left" },
        { id: "m-opus", name: "Claude Opus", limit: 50, used: 0, modelLimit: "none" },
      ],
    };
    fakeApi(routes(maintainer, teamModelsPayments, limits));
    renderApp("/teams/2?tab=limits");
    const c = await card();
    expect(await within(c).findByRole("group", { name: "Claude Sonnet limit" })).toHaveTextContent("Model limit for all teams: room left");
    expect(within(c).getByRole("group", { name: "Claude Opus limit" })).not.toHaveTextContent("Model limit for all teams");
  });

  it("shows a viewer no card and asks for no limits", async () => {
    const asViewer: TeamModelsView = { ...teamModelsPayments, role: "viewer", canEdit: false, models: [{ ...teamModelQwen, baseURL: "" }] };
    const fake = fakeApi(routes(viewer, asViewer));
    renderApp("/teams/2?tab=limits");
    // A viewer has no Limits tab, so the address falls back to Repositories.
    await screen.findByRole("heading", { name: "Payments" });
    expect(screen.queryByRole("tab", { name: "Limits" })).not.toBeInTheDocument();
    expect(screen.queryByRole("region", { name: "Model limits" })).not.toBeInTheDocument();
    expect(fake.find("GET", "/api/teams/2/limits")).toHaveLength(0);
  });

  it("shows nothing when the server says the session may not see the limits", async () => {
    const fake = fakeApi({ ...routes(), "GET /api/teams/2/limits": { status: 403, body: { error: "only maintainers of this team see its model limits" } } });
    renderApp("/teams/2?tab=limits");
    await waitFor(() => expect(fake.find("GET", "/api/teams/2/limits")).toHaveLength(1));
    await waitFor(() => expect(screen.queryByRole("region", { name: "Model limits" })).not.toBeInTheDocument());
    expect(screen.queryByText("only maintainers of this team see its model limits")).not.toBeInTheDocument();
  });
});
