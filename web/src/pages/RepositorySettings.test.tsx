import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import type { RepositoryDetailView } from "../api";
import { fakeApi } from "../test/fakeApi";
import { librarySkills, repoDetail, repoPayments, repoTeams, repoWidgets, ssoAdmin, viewer } from "../test/fixtures";
import { renderApp } from "../test/render";

afterEach(() => vi.unstubAllGlobals());

const noInputs = { branch: "main", skills: [], detected: [], instructions: "" };

const base = {
  "GET /api/me": { body: ssoAdmin },
  "GET /api/repositories/main/acme/payments": { body: repoDetail(repoPayments) },
  "GET /api/repositories/main/acme/widgets": { body: repoDetail(repoWidgets) },
  "GET /api/skills": { body: librarySkills },
  "GET /api/repositories/main/acme/payments/inputs": { body: noInputs },
  "GET /api/repositories/main/acme/widgets/inputs": { body: noInputs },
  "GET /api/repositories/main/acme/payments/teams": { body: repoTeams },
  "GET /api/repositories/main/acme/widgets/teams": { body: repoTeams },
};

const teamsSection = () => screen.findByRole("region", { name: "Teams" });

describe("RepositorySettings: Teams", () => {
  it("shows an admin every team, ticked when it lists the repository, and the whole-instance ones as such", async () => {
    fakeApi(base);
    renderApp("/repos/main/acme/payments?tab=settings");
    const box = await teamsSection();
    await within(box).findByRole("checkbox", { name: /Payments/ });
    expect(within(box).getByRole("checkbox", { name: /Payments/ })).toBeChecked();
    expect(within(box).getByRole("checkbox", { name: /Platform/ })).not.toBeChecked();
    const everyone = within(box).getByRole("checkbox", { name: /Everyone on main/ });
    expect(everyone).not.toBeChecked();
    expect(box).toHaveTextContent("covers every repository of main");
    expect(box).toHaveTextContent("6 members");
  });

  it("saves the ticked teams with a PUT and says so", async () => {
    const saved = { teams: repoTeams.teams.map((t) => (t.id === 3 ? { ...t, direct: true } : t)) };
    const fake = fakeApi({ ...base, "PUT /api/repositories/main/acme/payments/teams": { body: saved } });
    renderApp("/repos/main/acme/payments?tab=settings");
    const box = await teamsSection();
    await userEvent.click(await within(box).findByRole("checkbox", { name: /Platform/ }));
    await userEvent.click(within(box).getByRole("button", { name: "Save teams" }));
    const [put] = fake.find("PUT", "/api/repositories/main/acme/payments/teams");
    expect(put.body).toEqual({ teams: [2, 3] });
    expect(put.headers.get("X-CSRF-Token")).toBe("csrf-123");
    expect(await within(box).findByRole("status")).toHaveTextContent("Saved.");
    expect(within(box).getByRole("checkbox", { name: /Platform/ })).toBeChecked();
  });

  it("can take the repository off every team", async () => {
    const fake = fakeApi({
      ...base,
      "PUT /api/repositories/main/acme/payments/teams": { body: { teams: repoTeams.teams.map((t) => ({ ...t, direct: false })) } },
    });
    renderApp("/repos/main/acme/payments?tab=settings");
    const box = await teamsSection();
    await userEvent.click(await within(box).findByRole("checkbox", { name: /Payments/ }));
    await userEvent.click(within(box).getByRole("button", { name: "Save teams" }));
    expect(fake.find("PUT", "/api/repositories/main/acme/payments/teams")[0].body).toEqual({ teams: [] });
  });

  it("shows the server's text when a save is refused", async () => {
    fakeApi({
      ...base,
      "PUT /api/repositories/main/acme/payments/teams": { status: 400, body: { error: "every team must be a team that exists" } },
    });
    renderApp("/repos/main/acme/payments?tab=settings");
    const box = await teamsSection();
    await userEvent.click(await within(box).findByRole("checkbox", { name: /Platform/ }));
    await userEvent.click(within(box).getByRole("button", { name: "Save teams" }));
    expect(await within(box).findByRole("alert")).toHaveTextContent("every team must be a team that exists");
  });

  it("is there for a repository the config sets up, whose settings are locked", async () => {
    fakeApi(base);
    renderApp("/repos/main/acme/widgets?tab=settings");
    const box = await teamsSection();
    expect(await within(box).findByRole("checkbox", { name: /Payments/ })).toBeEnabled();
    expect(screen.getByText(/listed in pergent.sh's config/)).toBeInTheDocument();
  });

  it("points to Teams when there is none", async () => {
    fakeApi({ ...base, "GET /api/repositories/main/acme/payments/teams": { body: { teams: [] } } });
    renderApp("/repos/main/acme/payments?tab=settings");
    const box = await teamsSection();
    expect(await within(box).findByText(/No team yet/)).toBeInTheDocument();
    expect(within(box).getByRole("link", { name: /Teams/ })).toHaveAttribute("href", "/admin/teams");
  });

  it("is absent for a maintainer, and nothing asks for the teams", async () => {
    const fake = fakeApi({
      ...base,
      "GET /api/me": { body: viewer },
      "GET /api/repositories/main/acme/payments": { body: repoDetail(repoPayments, "maintainer") },
    });
    renderApp("/repos/main/acme/payments?tab=settings");
    await screen.findByRole("switch", { name: "Approve clean reviews" });
    expect(screen.queryByRole("region", { name: "Teams" })).not.toBeInTheDocument();
    expect(fake.calls.filter((c) => c.url.endsWith("/teams"))).toHaveLength(0);
  });
});

describe("RepositorySettings: Model", () => {
  const payments = "/api/repositories/main/acme/payments";
  const withModel = (d: RepositoryDetailView, id: string | null, label: string): RepositoryDetailView => ({
    ...d,
    model: label,
    settings: { ...d.settings, model: { value: id, overridden: id !== null } },
  });
  const routes = (detail: RepositoryDetailView, me = ssoAdmin) => ({
    ...base,
    "GET /api/me": { body: me },
    [`GET ${payments}`]: { body: detail },
    [`PUT ${payments}/settings`]: { body: detail },
  });
  const settingsPath = `${payments}/settings`;

  it("offers an admin the default, named, first, then every model of the list", async () => {
    fakeApi(routes(repoDetail(repoPayments)));
    renderApp("/repos/main/acme/payments?tab=settings");
    const select = await screen.findByLabelText("Model");
    expect(select).toBeEnabled();
    expect(select).toHaveValue("");
    expect(within(select).getAllByRole("option").map((o) => o.textContent)).toEqual([
      "Default (anthropic / claude-sonnet)",
      "anthropic / claude-sonnet",
      "anthropic / claude-opus",
    ]);
  });

  it("names the default on the first option also when the repository names its own model", async () => {
    fakeApi(routes(withModel(repoDetail(repoPayments), "m-opus", "anthropic / claude-opus")));
    renderApp("/repos/main/acme/payments?tab=settings");
    const select = await screen.findByLabelText("Model");
    expect(within(select).getAllByRole("option")[0]).toHaveTextContent("Default (anthropic / claude-sonnet)");
  });

  it("says plain Default when no choice is the default", async () => {
    const detail = repoDetail(repoPayments);
    fakeApi(routes({ ...detail, modelChoices: detail.modelChoices.map((c) => ({ ...c, default: false })) }));
    renderApp("/repos/main/acme/payments?tab=settings");
    const select = await screen.findByLabelText("Model");
    expect(within(select).getAllByRole("option")[0]).toHaveTextContent(/^Default$/);
  });

  it("sends the ID of the model picked, and nothing else", async () => {
    const fake = fakeApi(routes(repoDetail(repoPayments)));
    renderApp("/repos/main/acme/payments?tab=settings");
    await userEvent.selectOptions(await screen.findByLabelText("Model"), "anthropic / claude-opus");
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(fake.find("PUT", settingsPath)[0].body).toEqual({ model: "m-opus" });
  });

  it("sends null to follow the default again", async () => {
    const fake = fakeApi(routes(withModel(repoDetail(repoPayments), "m-opus", "anthropic / claude-opus")));
    renderApp("/repos/main/acme/payments?tab=settings");
    const select = await screen.findByLabelText("Model");
    expect(select).toHaveValue("m-opus");
    await userEvent.selectOptions(select, "");
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(fake.find("PUT", settingsPath)[0].body).toEqual({ model: null });
  });

  it("doesn't send the model when it didn't change", async () => {
    const fake = fakeApi(routes(withModel(repoDetail(repoPayments), "m-opus", "anthropic / claude-opus")));
    renderApp("/repos/main/acme/payments?tab=settings");
    await userEvent.click(await screen.findByRole("switch", { name: "Review drafts" }));
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(fake.find("PUT", settingsPath)[0].body).toEqual({ reviewDrafts: true });
  });

  it("shows a maintainer the current model in a disabled select, and never saves it", async () => {
    const detail = withModel(repoDetail(repoPayments, "maintainer"), "m-opus", "anthropic / claude-opus");
    const fake = fakeApi(routes(detail, viewer));
    renderApp("/repos/main/acme/payments?tab=settings");
    const select = await screen.findByLabelText("Model");
    expect(select).toBeDisabled();
    expect(select).toHaveDisplayValue("anthropic / claude-opus");
    // A maintainer is given no choices.
    expect(within(select).getAllByRole("option")).toHaveLength(1);
    expect(screen.getByText("Only admins of this repository change the model.")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("switch", { name: "Review drafts" }));
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(fake.find("PUT", settingsPath)[0].body).toEqual({ reviewDrafts: true });
    expect(fake.find("PUT", settingsPath)[0].body).not.toHaveProperty("model");
  });

  it("shows a maintainer the default's model when the repository names none", async () => {
    fakeApi(routes(repoDetail(repoPayments, "maintainer"), viewer));
    renderApp("/repos/main/acme/payments?tab=settings");
    expect(await screen.findByLabelText("Model")).toHaveDisplayValue("anthropic / claude-sonnet");
  });

  it("is disabled for a repository the config sets up", async () => {
    fakeApi({ ...routes(repoDetail(repoWidgets)), [`GET /api/repositories/main/acme/widgets`]: { body: repoDetail(repoWidgets) } });
    renderApp("/repos/main/acme/widgets?tab=settings");
    expect(await screen.findByLabelText("Model")).toBeDisabled();
  });

  it("says in a maintainer's reset confirmation that the model stays", async () => {
    fakeApi(routes(repoDetail(repoPayments, "maintainer"), viewer));
    const confirm = vi.spyOn(window, "confirm").mockReturnValue(false);
    renderApp("/repos/main/acme/payments?tab=settings");
    await userEvent.click(await screen.findByRole("button", { name: "Reset to defaults" }));
    expect(confirm).toHaveBeenCalledWith(expect.stringContaining("who is trusted and the model"));
    confirm.mockRestore();
  });
});
