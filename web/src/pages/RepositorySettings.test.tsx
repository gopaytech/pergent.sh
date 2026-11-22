import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import type { ChainView, CIJob, RepositoryDetailView } from "../api";
import { fakeApi } from "../test/fakeApi";
import {
  chainRepoCustom,
  chainRepoInherit,
  choice,
  entryConfig,
  entryOpus,
  entryTeamQwen,
  librarySkills,
  maintainer,
  panelRepoInherit,
  myTeams,
  repoDetail,
  repoGadgets,
  repoPayments,
  repoTeams,
  repoWidgets,
  ssoAdmin,
  viewer,
  ruleSets,
} from "../test/fixtures";
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
  "GET /api/repositories/main/acme/payments/chain": { body: chainRepoInherit },
  "GET /api/repositories/main/acme/widgets/chain": { body: chainRepoInherit },
  "GET /api/repositories/main/acme/payments/panel-chain": { body: panelRepoInherit },
  "GET /api/repositories/main/acme/widgets/panel-chain": { body: panelRepoInherit },
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

describe("RepositorySettings: Model chain", () => {
  const payments = "/api/repositories/main/acme/payments";
  const chainPath = `${payments}/chain`;
  const routes = (detail: RepositoryDetailView, me = ssoAdmin, chain: ChainView = chainRepoInherit) => ({
    ...base,
    "GET /api/me": { body: me },
    [`GET ${payments}`]: { body: detail },
    [`PUT ${payments}/settings`]: { body: detail },
    [`GET ${chainPath}`]: { body: chain },
  });
  const card = () => screen.findByRole("region", { name: "Model chain" });
  const names = (box: HTMLElement) =>
    within(within(box).getByRole("list", { name: "Model chain" }))
      .getAllByRole("listitem")
      .map((r) => r.querySelector("b")?.textContent);

  it("has no model menu any more, and sends no model in the settings", async () => {
    const fake = fakeApi(routes(repoDetail(repoPayments)));
    renderApp("/repos/main/acme/payments?tab=settings");
    await userEvent.click(await screen.findByRole("switch", { name: "Quiet reviews" }));
    expect(screen.queryByRole("combobox", { name: "Model" })).not.toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(fake.find("PUT", `${payments}/settings`)[0].body).toEqual({ quietReviews: true });
  });

  it("shows the chain the repository inherits and where it comes from", async () => {
    fakeApi(routes(repoDetail(repoPayments)));
    renderApp("/repos/main/acme/payments?tab=settings");
    const box = await card();
    expect(await within(box).findByText("Inherited from the chain of its owner team, Payments.")).toBeInTheDocument();
    expect(names(box)).toEqual(["qwen3.5", "Claude Sonnet"]);
    expect(within(box).queryByRole("button", { name: "Save chain" })).not.toBeInTheDocument();
  });

  it("says when the repository inherits the global chain", async () => {
    fakeApi(routes(repoDetail(repoPayments), ssoAdmin, { ...chainRepoInherit, source: "global", team: "" }));
    renderApp("/repos/main/acme/payments?tab=settings");
    expect(await within(await card()).findByText("Inherited from the global chain.")).toBeInTheDocument();
  });

  it("gives the repository its own chain, starting from the one in effect, with a PUT of the entries", async () => {
    const fake = fakeApi({ ...routes(repoDetail(repoPayments)), [`PUT ${chainPath}`]: { body: chainRepoCustom } });
    renderApp("/repos/main/acme/payments?tab=settings");
    const box = await card();
    await userEvent.click(await within(box).findByRole("button", { name: "Give it its own chain" }));
    await userEvent.click(within(box).getByRole("button", { name: "Remove Claude Sonnet" }));
    await userEvent.selectOptions(within(box).getByRole("combobox", { name: "Entry to add" }), "Claude Opus (global)");
    await userEvent.click(within(box).getByRole("button", { name: "Add" }));
    await userEvent.click(within(box).getByRole("button", { name: "Save chain" }));
    const [put] = fake.find("PUT", chainPath);
    expect(put.headers.get("X-CSRF-Token")).toBe("csrf-123");
    expect(put.body).toEqual({
      entries: [
        { source: "team", id: "t-qwen" },
        { source: "global", id: "m-opus" },
      ],
    });
    expect(await within(box).findByText("This repository has its own chain.")).toBeInTheDocument();
  });

  it("puts a repository's own chain back to inherit with a POST, after a confirmation", async () => {
    const confirm = vi.fn(() => true);
    vi.stubGlobal("confirm", confirm);
    const fake = fakeApi({
      ...routes(repoDetail(repoPayments), ssoAdmin, chainRepoCustom),
      [`POST ${chainPath}/reset`]: { body: chainRepoInherit },
    });
    renderApp("/repos/main/acme/payments?tab=settings");
    const box = await card();
    expect(await within(box).findByText("This repository has its own chain.")).toBeInTheDocument();
    await userEvent.click(within(box).getByRole("button", { name: "Inherit again" }));
    expect(confirm).toHaveBeenCalledWith(expect.stringContaining("owner team's chain, or the global chain"));
    const [post] = fake.find("POST", `${chainPath}/reset`);
    expect(post.headers.get("X-CSRF-Token")).toBe("csrf-123");
    expect(await within(box).findByText("Inherited from the chain of its owner team, Payments.")).toBeInTheDocument();
  });

  it("keeps a config file's entry movable in a repository's chain, and sends it", async () => {
    const config = { ...entryConfig, position: 2 };
    const chain: ChainView = { ...chainRepoCustom, entries: [{ ...entryOpus, position: 1 }, config], choices: [choice(entryOpus), choice(entryConfig)] };
    const fake = fakeApi({ ...routes(repoDetail(repoPayments), ssoAdmin, chain), [`PUT ${chainPath}`]: { body: chain } });
    renderApp("/repos/main/acme/payments?tab=settings");
    const box = await card();
    await userEvent.click(await within(box).findByRole("button", { name: "Move openai/gpt-5 up" }));
    await userEvent.click(within(box).getByRole("button", { name: "Save chain" }));
    expect(fake.find("PUT", chainPath)[0].body).toEqual({
      entries: [
        { source: "global", id: "config" },
        { source: "global", id: "m-opus" },
      ],
    });
  });

  it("flags the entries an admin saved that a maintainer may not keep", async () => {
    const chain: ChainView = { ...chainRepoCustom, entries: [{ ...entryOpus, position: 1 }, { ...entryTeamQwen, position: 2 }], choices: [choice(entryTeamQwen)] };
    fakeApi(routes(repoDetail(repoPayments, "maintainer"), viewer, chain));
    renderApp("/repos/main/acme/payments?tab=settings");
    const box = await card();
    expect(await within(box).findByText("An admin saved this chain.")).toBeInTheDocument();
    expect(within(box).getByText("You can't keep this entry here. Remove it to save.")).toBeInTheDocument();
    expect(within(box).getByRole("button", { name: "Save chain" })).toBeDisabled();
  });

  it("shows a maintainer of another team the chain with no button that changes it", async () => {
    // The server says read-only: the session doesn't maintain the owner team.
    fakeApi(routes(repoDetail(repoPayments, "maintainer"), viewer, { ...chainRepoCustom, readOnly: true, choices: [] }));
    renderApp("/repos/main/acme/payments?tab=settings");
    const box = await card();
    expect(await within(box).findByText("This repository has its own chain.")).toBeInTheDocument();
    expect(within(box).queryByRole("button")).not.toBeInTheDocument();
    expect(box).toHaveTextContent("Only admins and maintainers of the repository's owner team change its chain.");
  });

  it("shows a viewer the chain, read-only, on the Skills & instructions tab", async () => {
    fakeApi({
      ...routes(repoDetail(repoPayments, "viewer"), viewer, { ...chainRepoInherit, readOnly: true, choices: [] }),
    });
    renderApp("/repos/main/acme/payments?tab=inputs");
    const box = await card();
    expect(await within(box).findByText("Inherited from the chain of its owner team, Payments.")).toBeInTheDocument();
    expect(names(box)).toEqual(["qwen3.5", "Claude Sonnet"]);
    expect(within(box).queryByRole("button")).not.toBeInTheDocument();
  });

  it("clears a global entry's mark with the admin route", async () => {
    const marked = { ...entryOpus, position: 1, state: "exhausted" as const, class: "quota" as const, until: "2026-11-21T14:00:00Z", canClear: true };
    const fake = fakeApi({
      ...routes(repoDetail(repoPayments), ssoAdmin, { ...chainRepoCustom, entries: [marked] }),
      "DELETE /api/admin/models/m-opus/mark": { status: 204 },
    });
    renderApp("/repos/main/acme/payments?tab=settings");
    await userEvent.click(await within(await card()).findByRole("button", { name: "Clear mark of Claude Opus" }));
    expect(fake.find("DELETE", "/api/admin/models/m-opus/mark")).toHaveLength(1);
  });

  it("clears a team entry's mark with the route of its team", async () => {
    const marked = { ...entryTeamQwen, position: 1, state: "key_rejected" as const, class: "auth" as const, canClear: true };
    const fake = fakeApi({
      ...routes(repoDetail(repoPayments, "maintainer"), maintainer, { ...chainRepoInherit, entries: [marked] }),
      "GET /api/me/teams": { body: myTeams },
      "DELETE /api/teams/2/models/t-qwen/mark": { status: 204 },
    });
    renderApp("/repos/main/acme/payments?tab=settings");
    await userEvent.click(await within(await card()).findByRole("button", { name: "Clear mark of qwen3.5" }));
    await vi.waitFor(() => expect(fake.find("DELETE", "/api/teams/2/models/t-qwen/mark")).toHaveLength(1));
  });

  it("says in a maintainer's reset confirmation that the chain stays", async () => {
    fakeApi(routes(repoDetail(repoPayments, "maintainer"), viewer));
    const confirm = vi.spyOn(window, "confirm").mockReturnValue(false);
    renderApp("/repos/main/acme/payments?tab=settings");
    await userEvent.click(await screen.findByRole("button", { name: "Reset to defaults" }));
    expect(confirm).toHaveBeenCalledWith(expect.stringContaining("who is trusted and the token quota"));
    expect(confirm).toHaveBeenCalledWith(expect.stringContaining("The model chain stays as it is."));
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

describe("RepositorySettings: path rules", () => {
  const settingsPath = "/api/repositories/main/acme/payments/settings";

  it("chooses rule sets in order and sends them", async () => {
    const fake = fakeApi({ ...base, "GET /api/rule-sets": { body: ruleSets }, [`PUT ${settingsPath}`]: { body: repoDetail(repoPayments) } });
    renderApp("/repos/main/acme/payments?tab=settings");
    const card = await screen.findByRole("region", { name: "Rule sets used for this repository" });
    await userEvent.click(within(card).getByRole("radio", { name: "Choose rule sets" }));
    await userEvent.selectOptions(await within(card).findByRole("combobox", { name: "Add a rule set" }), "go-api");
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(fake.find("PUT", settingsPath)[0].body).toEqual({ ruleSets: ["go-api"] });
  });

  it("goes back to the default list with null, and refuses an empty choice", async () => {
    const own = repoDetail(repoPayments);
    own.settings.ruleSets = { value: ["go-api"], overridden: true };
    const fake = fakeApi({
      ...base,
      "GET /api/repositories/main/acme/payments": { body: own },
      "GET /api/rule-sets": { body: ruleSets },
      [`PUT ${settingsPath}`]: { body: repoDetail(repoPayments) },
    });
    renderApp("/repos/main/acme/payments?tab=settings");
    const card = await screen.findByRole("region", { name: "Rule sets used for this repository" });
    await userEvent.click(await within(card).findByRole("button", { name: "Remove go-api" }));
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(await screen.findByText("Choose at least one rule set, or use the default rule sets.")).toBeInTheDocument();
    await userEvent.click(within(card).getByRole("radio", { name: /Use the default rule sets/ }));
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(fake.find("PUT", settingsPath)[0].body).toEqual({ ruleSets: null });
  });

  it("sends only builtInRules when the switch is turned on", async () => {
    const fake = fakeApi({ ...base, "GET /api/rule-sets": { body: ruleSets }, [`PUT ${settingsPath}`]: { body: repoDetail(repoPayments) } });
    renderApp("/repos/main/acme/payments?tab=settings");
    // Built-in rules are opt-in: a repository that sets nothing has them off.
    const toggle = await screen.findByRole("switch", { name: "Built-in rules" });
    expect(toggle).not.toBeChecked();
    expect(screen.getByText(/Off by default: turn it on to use them\./)).toBeInTheDocument();
    await userEvent.click(toggle);
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(fake.find("PUT", settingsPath)[0].body).toEqual({ builtInRules: true });
  });
});
