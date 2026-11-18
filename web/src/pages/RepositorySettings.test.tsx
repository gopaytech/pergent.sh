import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import type { CIJob, RepositoryDetailView } from "../api";
import { fakeApi } from "../test/fakeApi";
import { librarySkills, repoDetail, repoGadgets, repoPayments, repoTeams, repoWidgets, ssoAdmin, viewer } from "../test/fixtures";
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
    expect(confirm).toHaveBeenCalledWith(expect.stringContaining("who is trusted, the model and the token quota"));
    confirm.mockRestore();
  });
});

describe("RepositorySettings: Review deleted and renamed files", () => {
  const help =
    "Reviews the lines a change deletes, and checks what still uses deleted files and renamed files' old paths. Costs more tokens. Needs pergent v0.10.0 or newer; an older image ignores it.";

  it("shows the switch after Quiet reviews, off, with its help text", async () => {
    fakeApi(base);
    renderApp("/repos/main/acme/payments?tab=settings");
    const toggle = await screen.findByRole("switch", { name: "Review deleted and renamed files" });
    expect(toggle).not.toBeChecked();
    expect(screen.getByText(help)).toBeInTheDocument();
    const switches = screen.getAllByRole("switch").map((el) => el.getAttribute("aria-label") ?? el.textContent);
    expect(switches.indexOf("Review deleted and renamed files")).toBe(switches.indexOf("Quiet reviews") + 1);
  });

  it("sends only checkDeleted when it is switched on", async () => {
    const fake = fakeApi({
      ...base,
      "PUT /api/repositories/main/acme/payments/settings": { body: repoDetail(repoPayments) },
    });
    renderApp("/repos/main/acme/payments?tab=settings");
    await userEvent.click(await screen.findByRole("switch", { name: "Review deleted and renamed files" }));
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(fake.find("PUT", "/api/repositories/main/acme/payments/settings")[0].body).toEqual({ checkDeleted: true });
  });

  it("shows it on when the repository sets it", async () => {
    const own = repoDetail(repoPayments);
    own.settings.checkDeleted = { value: true, overridden: true };
    fakeApi({ ...base, "GET /api/repositories/main/acme/payments": { body: own } });
    renderApp("/repos/main/acme/payments?tab=settings");
    expect(await screen.findByRole("switch", { name: "Review deleted and renamed files" })).toBeChecked();
  });
});

describe("RepositorySettings: CI jobs", () => {
  const payments = "/api/repositories/main/acme/payments";
  const settingsPath = `${payments}/settings`;
  const withJobs = (d: RepositoryDetailView, wait: string, jobs: CIJob[]): RepositoryDetailView => ({
    ...d,
    settings: { ...d.settings, ciJobs: { value: { wait, jobs }, overridden: jobs.length > 0 } },
  });
  const routes = (detail: RepositoryDetailView, extra: Record<string, unknown> = {}) => ({
    ...base,
    [`GET ${payments}`]: { body: detail },
    [`PUT ${settingsPath}`]: { body: detail },
    [`GET ${payments}/ci-job-names`]: { body: { names: ["build", "test"] } },
    ...extra,
  });
  const card = () => screen.findByRole("region", { name: "CI jobs" });

  it("shows the card with the default wait and no job, for a maintainer too", async () => {
    fakeApi({ ...routes(repoDetail(repoPayments, "maintainer")), "GET /api/me": { body: viewer } });
    renderApp("/repos/main/acme/payments?tab=settings");
    const box = await card();
    expect(within(box).getByRole("textbox", { name: "How long to wait for CI" })).toHaveValue("30m");
    expect(within(box).getByText("No CI job yet.")).toBeInTheDocument();
    expect(within(box).getByText("default")).toBeInTheDocument();
  });

  it("suggests the names the server lists, and takes free text", async () => {
    const fake = fakeApi(routes(repoDetail(repoPayments)));
    renderApp("/repos/main/acme/payments?tab=settings");
    const box = await card();
    await userEvent.click(within(box).getByRole("button", { name: "Add a job" }));
    await waitFor(() => expect(box.querySelectorAll("datalist option")).toHaveLength(2));
    expect([...box.querySelectorAll("datalist option")].map((o) => o.getAttribute("value"))).toEqual(["build", "test"]);
    expect(fake.find("GET", `${payments}/ci-job-names`)).toHaveLength(1);
    await userEvent.type(within(box).getByRole("combobox", { name: "Job name" }), "deploy-docs");
    expect(within(box).getByText("Not seen in the last 10 runs. Check the name.")).toBeInTheDocument();
  });

  it("gives no unknown-name warning when the names could not be read", async () => {
    fakeApi(routes(repoDetail(repoPayments), { [`GET ${payments}/ci-job-names`]: { status: 502, body: { error: "no" } } }));
    renderApp("/repos/main/acme/payments?tab=settings");
    const box = await card();
    await userEvent.click(within(box).getByRole("button", { name: "Add a job" }));
    await userEvent.type(within(box).getByRole("combobox", { name: "Job name" }), "deploy-docs");
    expect(within(box).queryByText("Not seen in the last 10 runs. Check the name.")).not.toBeInTheDocument();
  });

  it("sends ciJobs, and only that, when a job is added", async () => {
    const fake = fakeApi(routes(repoDetail(repoPayments)));
    renderApp("/repos/main/acme/payments?tab=settings");
    const box = await card();
    await userEvent.click(within(box).getByRole("button", { name: "Add a job" }));
    await userEvent.type(within(box).getByRole("combobox", { name: "Job name" }), " test ");
    await userEvent.selectOptions(within(box).getByRole("combobox", { name: "Read" }), "log");
    await userEvent.click(within(box).getByRole("switch", { name: "Blocks approval" }));
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(fake.find("PUT", settingsPath)[0].body).toEqual({
      ciJobs: { wait: "30m", jobs: [{ name: "test", read: "log", blocksApproval: true }] },
    });
  });

  it("sends the GitHub artifact only on GitHub", async () => {
    const hub = repoDetail(repoGadgets);
    const fake = fakeApi({
      ...base,
      "GET /api/repositories/hub/acme/gadgets": { body: hub },
      "PUT /api/repositories/hub/acme/gadgets/settings": { body: hub },
      "GET /api/repositories/hub/acme/gadgets/inputs": { body: noInputs },
      "GET /api/repositories/hub/acme/gadgets/teams": { body: repoTeams },
      "GET /api/repositories/hub/acme/gadgets/ci-job-names": { body: { names: [] } },
      "GET /api/admin/linkable?instance=hub&repo=acme%2Fgadgets&search=": { body: { items: [], truncated: false } },
    });
    renderApp("/repos/hub/acme/gadgets?tab=settings");
    const box = await card();
    await userEvent.click(within(box).getByRole("button", { name: "Add a job" }));
    await userEvent.type(within(box).getByRole("combobox", { name: "Job name" }), "scan");
    await userEvent.selectOptions(within(box).getByRole("combobox", { name: "Read" }), "report");
    await userEvent.type(within(box).getByRole("textbox", { name: "Report artifact" }), "scan-out");
    await userEvent.type(within(box).getByRole("textbox", { name: "Report path" }), "scan.json");
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(fake.find("PUT", "/api/repositories/hub/acme/gadgets/settings")[0].body).toEqual({
      ciJobs: { wait: "30m", jobs: [{ name: "scan", read: "report", report: { artifact: "scan-out", path: "scan.json" }, blocksApproval: false }] },
    });
  });

  it("sends null when the last job is removed, and nothing when nothing changed", async () => {
    const own = withJobs(repoDetail(repoPayments), "45m", [{ name: "test", read: "status", blocksApproval: true }]);
    const fake = fakeApi(routes(own));
    renderApp("/repos/main/acme/payments?tab=settings");
    const box = await card();
    expect(within(box).getByText("overridden here")).toBeInTheDocument();
    expect(within(box).getByRole("textbox", { name: "How long to wait for CI" })).toHaveValue("45m");
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(fake.find("PUT", settingsPath)).toHaveLength(0);
    await userEvent.click(within(box).getByRole("button", { name: "Remove test" }));
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(fake.find("PUT", settingsPath)[0].body).toEqual({ ciJobs: null });
  });

  it("shows the wait of a repository that has no job, and sends a changed wait with its jobs only", async () => {
    const own = withJobs(repoDetail(repoPayments), "45m", [{ name: "test", read: "status", blocksApproval: false }]);
    const fake = fakeApi(routes(own));
    renderApp("/repos/main/acme/payments?tab=settings");
    const box = await card();
    const wait = within(box).getByRole("textbox", { name: "How long to wait for CI" });
    await userEvent.clear(wait);
    await userEvent.type(wait, "1h");
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(fake.find("PUT", settingsPath)[0].body).toEqual({
      ciJobs: { wait: "1h", jobs: [{ name: "test", read: "status", blocksApproval: false }] },
    });
  });

  it("is read-only for a repository the config sets up, and asks for no names", async () => {
    const widgets = withJobs(repoDetail(repoWidgets), "10m", [{ name: "build", read: "log", blocksApproval: true }]);
    const fake = fakeApi({ ...base, "GET /api/repositories/main/acme/widgets": { body: widgets } });
    renderApp("/repos/main/acme/widgets?tab=settings");
    const box = await card();
    expect(within(box).getByText("managed by config")).toBeInTheDocument();
    expect(within(box).getByRole("textbox", { name: "How long to wait for CI" })).toBeDisabled();
    expect(within(box).getByRole("combobox", { name: "Job name" })).toBeDisabled();
    expect(within(box).getByRole("button", { name: "Add a job" })).toBeDisabled();
    expect(within(box).getByRole("button", { name: "Remove build" })).toBeDisabled();
    expect(fake.calls.filter((c) => c.url.endsWith("/ci-job-names"))).toHaveLength(0);
  });
});
