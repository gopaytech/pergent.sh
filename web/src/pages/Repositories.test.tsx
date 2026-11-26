import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { fakeApi } from "../test/fakeApi";
import {
  instanceDashboardGitLab,
  instanceGitLab,
  librarySkills,
  maintainer,
  myTeams,
  operator,
  ssoAdmin,
  repoDetail,
  repoPayments,
  repoToolsPaused,
  repoWidgets,
  teamEmpty,
  teamPayments,
  viewer,
} from "../test/fixtures";
import { renderApp } from "../test/render";

afterEach(() => vi.unstubAllGlobals());

describe("Repositories", () => {
  it("lists the repositories with platform, source, last 7 days and health", async () => {
    fakeApi({ "GET /api/me": { body: viewer }, "GET /api/repositories": { body: [repoPayments, repoWidgets] } });
    renderApp("/repos");
    expect(await screen.findByRole("heading", { name: "Repositories" })).toBeInTheDocument();
    const payments = (await screen.findByRole("link", { name: "acme/payments" })).closest("tr") as HTMLElement;
    expect(within(payments).getByRole("link", { name: "acme/payments" })).toHaveAttribute("href", "/repos/main/acme/payments");
    expect(payments).toHaveTextContent("GitLab");
    expect(payments).toHaveTextContent("40");
    expect(payments).toHaveTextContent("31");
    expect(within(payments).getByText("Needs attention")).toHaveClass("pill", "warn");
    const widgets = screen.getByRole("link", { name: "acme/widgets" }).closest("tr") as HTMLElement;
    expect(within(widgets).getByText("managed by config")).toBeInTheDocument();
    expect(within(widgets).getByText("OK")).toHaveClass("pill", "ok");
    // Developers can't switch anything.
    expect(screen.queryByRole("button", { name: "+ Add repository" })).not.toBeInTheDocument();
    expect(screen.queryByRole("switch")).not.toBeInTheDocument();
  });

  it("asks an admin for the instance first, then lists that instance's repositories, and switches one on with the CSRF token", async () => {
    const fake = fakeApi({
      "GET /api/me": { body: operator },
      "GET /api/repositories": { body: [repoWidgets] },
      "GET /api/admin/instances": { body: [instanceGitLab, { ...instanceDashboardGitLab, removing: true }] },
      "GET /api/admin/instances/main/repositories?search=": {
        body: { items: [{ instance: "main", project: "acme/payments", platform: "gitlab", state: "off", ownerTeam: "" }], page: 1, hasMore: false, truncated: false, instanceOwner: "" },
      },
      "GET /api/admin/instances/main/repositories?search=pay": {
        body: { items: [{ instance: "main", project: "acme/payments", platform: "gitlab", state: "off", ownerTeam: "" }], page: 1, hasMore: false, truncated: false, instanceOwner: "" },
      },
      "POST /api/repositories": { status: 201, body: repoDetail(repoPayments) },
    });
    renderApp("/repos");
    await userEvent.click(await screen.findByRole("button", { name: "+ Add repository" }));
    const picker = await screen.findByRole("region", { name: "Add repository" });
    const choose = await within(picker).findByLabelText("Instance");
    // Nothing is listed before one is chosen.
    expect(within(picker).queryByRole("searchbox")).not.toBeInTheDocument();
    expect(fake.calls.filter((c) => c.url.includes("/repositories?search="))).toHaveLength(0);
    // An instance being removed isn't offered.
    expect(within(choose).queryByRole("option", { name: /extra/ })).not.toBeInTheDocument();
    await userEvent.selectOptions(choose, "main");
    expect(await within(picker).findByText("acme/payments")).toBeInTheDocument();
    expect(fake.find("GET", "/api/repositories/available?search=")).toHaveLength(0);

    await userEvent.type(within(picker).getByRole("searchbox", { name: "Search" }), "pay");
    await userEvent.click(within(picker).getByRole("button", { name: "Search" }));
    await vi.waitFor(() => expect(fake.find("GET", "/api/admin/instances/main/repositories?search=pay")).toHaveLength(1));

    await userEvent.click(within(picker).getByRole("button", { name: "Switch on acme/payments" }));
    const [post] = fake.find("POST", "/api/repositories");
    expect(post.headers.get("X-CSRF-Token")).toBe("csrf-123");
    expect(post.body).toEqual({ project: "acme/payments", instance: "main" });
    // The page moves after the POST answers; wait for it, as findBy stops at the first match.
    await vi.waitFor(() => expect(screen.getByTestId("location")).toHaveTextContent("/repos/main/acme/payments"));
  });

  describe("the owner team select", () => {
    const routes = (teams = [teamPayments, teamEmpty]) => ({
      "GET /api/me": { body: operator },
      "GET /api/repositories": { body: [repoWidgets] },
      "GET /api/admin/instances": { body: [instanceGitLab] },
      "GET /api/teams": { body: teams },
      "GET /api/admin/instances/main/repositories?search=": {
        body: { items: [{ instance: "main", project: "acme/payments", platform: "gitlab", state: "off", ownerTeam: "" }], page: 1, hasMore: false, truncated: false, instanceOwner: "" },
      },
      "POST /api/repositories": { status: 201, body: repoDetail(repoPayments) },
    });

    it("lists None first, then every team, and sends the chosen team with the switch-on", async () => {
      const fake = fakeApi(routes());
      renderApp("/repos?add=1");
      const picker = await screen.findByRole("region", { name: "Add repository" });
      await userEvent.selectOptions(await within(picker).findByLabelText("Instance"), "main");
      const select = await within(picker).findByLabelText("Owner team");
      expect(within(select).getAllByRole("option").map((o) => o.textContent)).toEqual(["None", "Payments", "Platform"]);
      await userEvent.selectOptions(select, "Payments");
      await userEvent.click(await within(picker).findByRole("button", { name: "Switch on acme/payments" }));
      const [post] = fake.find("POST", "/api/repositories");
      expect(post.body).toEqual({ project: "acme/payments", instance: "main", ownerTeam: teamPayments.id });
    });

    it("reads the team lists and the owner team again after a switch-on with an owner team", async () => {
      fakeApi(routes());
      const { client } = renderApp("/repos?add=1");
      const spy = vi.spyOn(client, "invalidateQueries");
      const picker = await screen.findByRole("region", { name: "Add repository" });
      await userEvent.selectOptions(await within(picker).findByLabelText("Instance"), "main");
      await userEvent.selectOptions(await within(picker).findByLabelText("Owner team"), "Payments");
      await userEvent.click(await within(picker).findByRole("button", { name: "Switch on acme/payments" }));
      const keys = () => spy.mock.calls.map((c) => JSON.stringify((c[0] as { queryKey: unknown[] }).queryKey));
      await vi.waitFor(() => expect(keys()).toContain('["repositories"]'));
      for (const k of ["[\"teams\"]", "[\"my-teams\"]", `["team",${teamPayments.id}]`]) {
        expect(keys()).toContain(k);
      }
    });

    it("sends no ownerTeam with None", async () => {
      const fake = fakeApi(routes());
      renderApp("/repos?add=1");
      const picker = await screen.findByRole("region", { name: "Add repository" });
      await userEvent.selectOptions(await within(picker).findByLabelText("Instance"), "main");
      await within(picker).findByLabelText("Owner team");
      await userEvent.click(await within(picker).findByRole("button", { name: "Switch on acme/payments" }));
      const [post] = fake.find("POST", "/api/repositories");
      expect(post.body).toEqual({ project: "acme/payments", instance: "main" });
    });

    it("does not take the No owner filter as the owner team", async () => {
      const fake = fakeApi(routes());
      renderApp("/repos?team=none&add=1");
      const picker = await screen.findByRole("region", { name: "Add repository" });
      await userEvent.selectOptions(await within(picker).findByLabelText("Instance"), "main");
      expect(await within(picker).findByLabelText("Owner team")).toHaveDisplayValue("None");
      await userEvent.click(await within(picker).findByRole("button", { name: "Switch on acme/payments" }));
      const [post] = fake.find("POST", "/api/repositories");
      expect(post.body).toEqual({ project: "acme/payments", instance: "main" });
    });

    it("starts the owner team from a numeric team filter", async () => {
      fakeApi(routes());
      renderApp(`/repos?team=${teamPayments.id}&add=1`);
      const picker = await screen.findByRole("region", { name: "Add repository" });
      await userEvent.selectOptions(await within(picker).findByLabelText("Instance"), "main");
      expect(await within(picker).findByLabelText("Owner team")).toHaveValue(String(teamPayments.id));
    });

    it("says which team owns the whole instance today", async () => {
      const widgets = { ...teamEmpty, id: 4, name: "Widgets", repos: [{ instance: "main", project: "", owner: true }] };
      fakeApi(routes([teamPayments, widgets]));
      renderApp("/repos?add=1");
      const picker = await screen.findByRole("region", { name: "Add repository" });
      await userEvent.selectOptions(await within(picker).findByLabelText("Instance"), "main");
      expect(await within(picker).findByText("Owner today: Widgets (all repositories of main)")).toBeInTheDocument();
    });
  });

  it("says what to do when the repository can't be reached any more", async () => {
    fakeApi({
      "GET /api/me": { body: operator },
      "GET /api/repositories": { body: [repoWidgets] },
      "GET /api/admin/instances": { body: [instanceGitLab] },
      "GET /api/admin/instances/main/repositories?search=": {
        body: { items: [{ instance: "main", project: "acme/payments", platform: "gitlab", state: "off", ownerTeam: "" }], page: 1, hasMore: false, truncated: false, instanceOwner: "" },
      },
      "POST /api/repositories": { status: 404, body: { error: "not found" } },
    });
    renderApp("/repos?add=1");
    const picker = await screen.findByRole("region", { name: "Add repository" });
    await userEvent.selectOptions(await within(picker).findByLabelText("Instance"), "main");
    await userEvent.click(await within(picker).findByRole("button", { name: "Switch on acme/payments" }));
    expect(await screen.findByText(/can't reach this repository now/)).toBeInTheDocument();
    expect(screen.queryByText("not found")).not.toBeInTheDocument();
  });

  const gitlabPage = (from: number, count: number, page: number, hasMore: boolean, truncated = false) => ({
    items: Array.from({ length: count }, (_, i) => ({
      instance: "main",
      project: `acme/repo-${String(from + i).padStart(3, "0")}`,
      platform: "gitlab",
      state: "off",
      ownerTeam: "",
    })),
    page,
    hasMore,
    truncated,
    instanceOwner: "",
  });

  const openPicker = async () => {
    renderApp("/repos?add=1");
    const picker = await screen.findByRole("region", { name: "Add repository" });
    await userEvent.selectOptions(await within(picker).findByLabelText("Instance"), "main");
    return picker;
  };

  it("lists 50 at a time: Load more appends the next page and goes away on the last", async () => {
    const fake = fakeApi({
      "GET /api/me": { body: operator },
      "GET /api/repositories": { body: [repoWidgets] },
      "GET /api/admin/instances": { body: [instanceGitLab] },
      "GET /api/admin/instances/main/repositories?search=": { body: gitlabPage(0, 50, 1, true) },
      "GET /api/admin/instances/main/repositories?search=&page=2": { body: gitlabPage(50, 49, 2, true) },
      "GET /api/admin/instances/main/repositories?search=&page=3": { body: gitlabPage(99, 5, 3, false) },
    });
    const picker = await openPicker();
    expect(await within(picker).findByText("acme/repo-000")).toBeInTheDocument();
    expect(within(picker).getAllByRole("button", { name: /^Switch on / })).toHaveLength(50);

    await userEvent.click(within(picker).getByRole("button", { name: "Load more" }));
    expect(await within(picker).findByText("acme/repo-050")).toBeInTheDocument();
    // The first page stays, and the next is under it.
    expect(within(picker).getByText("acme/repo-000")).toBeInTheDocument();
    expect(within(picker).getAllByRole("button", { name: /^Switch on / })).toHaveLength(99);
    expect(fake.find("GET", "/api/admin/instances/main/repositories?search=&page=2")).toHaveLength(1);

    await userEvent.click(within(picker).getByRole("button", { name: "Load more" }));
    expect(await within(picker).findByText("acme/repo-103")).toBeInTheDocument();
    expect(within(picker).getAllByRole("button", { name: /^Switch on / })).toHaveLength(104);
    expect(within(picker).queryByRole("button", { name: "Load more" })).not.toBeInTheDocument();
    expect(within(picker).queryByText("No repository to switch on.")).not.toBeInTheDocument();
    // Nothing was read ahead.
    expect(fake.calls.filter((c) => c.url.includes("/repositories?"))).toHaveLength(3);
  });

  it("starts again at page 1 for a new search", async () => {
    const fake = fakeApi({
      "GET /api/me": { body: operator },
      "GET /api/repositories": { body: [repoWidgets] },
      "GET /api/admin/instances": { body: [instanceGitLab] },
      "GET /api/admin/instances/main/repositories?search=": { body: gitlabPage(0, 50, 1, true) },
      "GET /api/admin/instances/main/repositories?search=&page=2": { body: gitlabPage(50, 10, 2, false) },
      "GET /api/admin/instances/main/repositories?search=pay": {
        body: { items: [{ instance: "main", project: "acme/payments", platform: "gitlab", state: "off", ownerTeam: "" }], page: 1, hasMore: true, truncated: false, instanceOwner: "" },
      },
    });
    const picker = await openPicker();
    await within(picker).findByText("acme/repo-000");
    await userEvent.click(within(picker).getByRole("button", { name: "Load more" }));
    await within(picker).findByText("acme/repo-050");

    await userEvent.type(within(picker).getByRole("searchbox", { name: "Search" }), "pay");
    await userEvent.click(within(picker).getByRole("button", { name: "Search" }));
    expect(await within(picker).findByText("acme/payments")).toBeInTheDocument();
    // Only the search's first page: the old list is gone, and Load more is the search's.
    expect(within(picker).getAllByRole("button", { name: /^Switch on / })).toHaveLength(1);
    expect(within(picker).queryByText("acme/repo-000")).not.toBeInTheDocument();
    expect(within(picker).getByRole("button", { name: "Load more" })).toBeEnabled();
    expect(fake.find("GET", "/api/admin/instances/main/repositories?search=pay")).toHaveLength(1);
    expect(fake.calls.some((c) => c.url.includes("search=pay&page="))).toBe(false);
  });

  it("keeps the list and offers Load more again when the next page fails", async () => {
    let fail = true;
    fakeApi({
      "GET /api/me": { body: operator },
      "GET /api/repositories": { body: [repoWidgets] },
      "GET /api/admin/instances": { body: [instanceGitLab] },
      "GET /api/admin/instances/main/repositories?search=": { body: gitlabPage(0, 50, 1, true) },
      "GET /api/admin/instances/main/repositories?search=&page=2": () =>
        fail ? { status: 502, body: { error: "could not ask GitLab" } } : { body: gitlabPage(50, 1, 2, false) },
    });
    const picker = await openPicker();
    await within(picker).findByText("acme/repo-000");
    await userEvent.click(within(picker).getByRole("button", { name: "Load more" }));
    expect(await within(picker).findByText("could not ask GitLab")).toBeInTheDocument();
    expect(within(picker).getAllByRole("button", { name: /^Switch on / })).toHaveLength(50);

    fail = false;
    await userEvent.click(within(picker).getByRole("button", { name: "Load more" }));
    expect(await within(picker).findByText("acme/repo-050")).toBeInTheDocument();
    expect(within(picker).queryByText("could not ask GitLab")).not.toBeInTheDocument();
  });

  it("says when the list is cut short, and offers Load more after an empty page", async () => {
    fakeApi({
      "GET /api/me": { body: operator },
      "GET /api/repositories": { body: [repoWidgets] },
      "GET /api/admin/instances": { body: [instanceGitLab] },
      "GET /api/admin/instances/main/repositories?search=": { body: { items: [], page: 1, hasMore: true, truncated: true, instanceOwner: "" } },
    });
    const picker = await openPicker();
    expect(await within(picker).findByText(/too long to read whole/)).toBeInTheDocument();
    // A page can be empty and not the last: no "nothing" yet, but Load more.
    expect(within(picker).queryByText("No repository to switch on.")).not.toBeInTheDocument();
    expect(within(picker).getByText("Everything on this page is already switched on.")).toBeInTheDocument();
    expect(within(picker).getByRole("button", { name: "Load more" })).toBeInTheDocument();
  });

  it("says so when a later page is empty because everything on it is switched on", async () => {
    fakeApi({
      "GET /api/me": { body: operator },
      "GET /api/repositories": { body: [repoWidgets] },
      "GET /api/admin/instances": { body: [instanceGitLab] },
      "GET /api/admin/instances/main/repositories?search=": { body: gitlabPage(0, 3, 1, true) },
      "GET /api/admin/instances/main/repositories?search=&page=2": { body: gitlabPage(3, 0, 2, true) },
    });
    const picker = await openPicker();
    await within(picker).findByText("acme/repo-000");
    expect(within(picker).queryByText("Everything on this page is already switched on.")).not.toBeInTheDocument();
    await userEvent.click(within(picker).getByRole("button", { name: "Load more" }));
    expect(await within(picker).findByText("Everything on this page is already switched on.")).toBeInTheDocument();
    expect(within(picker).getAllByRole("button", { name: /^Switch on / })).toHaveLength(3);
  });

  it("warns on Add repository when the default skill list is empty", async () => {
    fakeApi({
      "GET /api/me": { body: ssoAdmin },
      "GET /api/repositories": { body: [repoWidgets] },
      "GET /api/admin/instances": { body: [instanceGitLab] },
      "GET /api/skills": { body: librarySkills.map((s) => ({ ...s, inDefaults: false })) },
    });
    renderApp("/repos?add=1");
    const picker = await screen.findByRole("region", { name: "Add repository" });
    const warning = await within(picker).findByRole("note");
    expect(warning).toHaveTextContent("No default skills are set");
    expect(warning).toHaveTextContent("gets no skills and is not reviewed");
    expect(within(warning).getByRole("link", { name: "Set the default skills" })).toHaveAttribute("href", "/admin/settings");
  });

  it("doesn't warn on Add repository when there are default skills", async () => {
    fakeApi({
      "GET /api/me": { body: ssoAdmin },
      "GET /api/repositories": { body: [repoWidgets] },
      "GET /api/admin/instances": { body: [instanceGitLab] },
      "GET /api/admin/instances/main/repositories?search=": { body: { items: [], page: 1, hasMore: false, truncated: false, instanceOwner: "" } },
      "GET /api/skills": { body: librarySkills },
    });
    renderApp("/repos?add=1");
    const picker = await screen.findByRole("region", { name: "Add repository" });
    await userEvent.selectOptions(await within(picker).findByLabelText("Instance"), "main");
    await within(picker).findByText("No repository to switch on.");
    expect(within(picker).queryByRole("note")).not.toBeInTheDocument();
  });

  it("opens the picker from the overview's Add repository", async () => {
    fakeApi({
      "GET /api/me": { body: ssoAdmin },
      "GET /api/repositories": { body: [] },
      "GET /api/admin/instances": { body: [instanceGitLab] },
      "GET /api/admin/instances/main/repositories?search=": { status: 502, body: { error: "could not ask GitLab" } },
    });
    renderApp("/repos?add=1");
    const picker = await screen.findByRole("region", { name: "Add repository" });
    await userEvent.selectOptions(await within(picker).findByLabelText("Instance"), "main");
    expect(await within(picker).findByText("could not ask GitLab")).toBeInTheDocument();
  });

  describe("the picker for maintainers", () => {
    const row = (project: string, state: "off" | "paused" | "on", ownerTeam = "", config = false) => ({
      instance: "main",
      project,
      platform: "gitlab",
      state,
      ownerTeam,
      config,
    });
    const page = (items: unknown[], instanceOwner = "") => ({ items, page: 1, hasMore: false, truncated: false, instanceOwner });
    const payments = { ...myTeams[0] };
    const routes = (items: unknown[], instanceOwner = "", teams = myTeams) => ({
      "GET /api/me": { body: maintainer },
      "GET /api/repositories": { body: [repoWidgets] },
      "GET /api/me/teams": { body: teams },
      "GET /api/repositories/available?instance=main&search=": { body: page(items, instanceOwner) },
      "POST /api/repositories": { status: 201, body: repoDetail(repoPayments, "maintainer") },
      "POST /api/repositories/main/acme/payments/claim": { body: repoDetail(repoPayments, "maintainer") },
      "PUT /api/admin/repositories/main/acme/payments": { body: repoDetail(repoPayments, "maintainer") },
    });
    const open = async () => {
      renderApp("/repos?add=1");
      const picker = await screen.findByRole("region", { name: "Add repository" });
      const choose = await within(picker).findByLabelText("Instance");
      // Only the instances the session may onboard on, with no admin read.
      expect(within(choose).getAllByRole("option").map((o) => o.textContent)).toEqual(["Choose an instance", "main"]);
      await userEvent.selectOptions(choose, "main");
      return picker;
    };

    it("shows + Add repository to a maintainer of an onboarding instance, and not to a viewer", async () => {
      fakeApi({ "GET /api/me": { body: maintainer }, "GET /api/repositories": { body: [repoWidgets] } });
      const first = renderApp("/repos");
      expect(await screen.findByRole("button", { name: "+ Add repository" })).toBeInTheDocument();
      first.unmount();
      fakeApi({ "GET /api/me": { body: viewer }, "GET /api/repositories": { body: [repoWidgets] } });
      renderApp("/repos?add=1");
      await screen.findByRole("heading", { name: "Repositories" });
      await screen.findByRole("link", { name: "acme/widgets" });
      expect(screen.queryByRole("button", { name: "+ Add repository" })).not.toBeInTheDocument();
      expect(screen.queryByRole("region", { name: "Add repository" })).not.toBeInTheDocument();
    });

    it("Onboard: an off row sends the maintainer's only team, and the team select is hidden", async () => {
      const fake = fakeApi(routes([row("acme/payments", "off")]));
      const picker = await open();
      await userEvent.click(await within(picker).findByRole("button", { name: "Onboard acme/payments" }));
      expect(within(picker).queryByLabelText("Owner team")).not.toBeInTheDocument();
      const [post] = fake.find("POST", "/api/repositories");
      expect(post.headers.get("X-CSRF-Token")).toBe("csrf-maint");
      expect(post.body).toEqual({ project: "acme/payments", instance: "main", ownerTeam: payments.id });
      expect(fake.find("GET", "/api/admin/instances")).toHaveLength(0);
      await vi.waitFor(() => expect(screen.getByTestId("location")).toHaveTextContent("/repos/main/acme/payments"));
    });

    it("Claim: an on row with no owner posts the chosen team and reads the lists again", async () => {
      const fake = fakeApi(routes([row("acme/payments", "on")]));
      const { client } = renderApp("/repos?add=1");
      const spy = vi.spyOn(client, "invalidateQueries");
      const picker = await screen.findByRole("region", { name: "Add repository" });
      await userEvent.selectOptions(await within(picker).findByLabelText("Instance"), "main");
      await userEvent.click(await within(picker).findByRole("button", { name: "Claim acme/payments" }));
      const [post] = fake.find("POST", "/api/repositories/main/acme/payments/claim");
      expect(post.body).toEqual({ team: payments.id });
      const keys = () => spy.mock.calls.map((c) => JSON.stringify((c[0] as { queryKey: unknown[] }).queryKey));
      await vi.waitFor(() => expect(keys()).toContain('["repositories"]'));
      for (const k of ['["available"]', '["my-teams"]', '["teams"]']) expect(keys()).toContain(k);
      await vi.waitFor(() => expect(screen.getByTestId("location")).toHaveTextContent("/repos/main/acme/payments"));
    });

    it("Claim: a paused row with no owner is also Claim", async () => {
      fakeApi(routes([row("acme/payments", "paused")]));
      const picker = await open();
      expect(await within(picker).findByRole("button", { name: "Claim acme/payments" })).toBeInTheDocument();
    });

    it("says managed by the config file, with no Claim, for a config row without an owner", async () => {
      fakeApi(routes([row("acme/payments", "on", "", true)]));
      const picker = await open();
      expect(await within(picker).findByText("managed by the config file")).toBeInTheDocument();
      expect(within(picker).queryByRole("button", { name: "Claim acme/payments" })).not.toBeInTheDocument();
    });

    it("Resume: a paused row owned by the maintainer's team puts it back with PUT enabled", async () => {
      const fake = fakeApi(routes([row("acme/payments", "paused", "Payments")]));
      const picker = await open();
      await userEvent.click(await within(picker).findByRole("button", { name: "Resume acme/payments" }));
      const [put] = fake.find("PUT", "/api/admin/repositories/main/acme/payments");
      expect(put.body).toEqual({ enabled: true });
      expect(within(picker).queryByRole("button", { name: "Claim acme/payments" })).not.toBeInTheDocument();
    });

    it("says owned by <team>, with no button, for a row another team owns", async () => {
      fakeApi(routes([row("acme/payments", "on", "Platform"), row("acme/tools", "paused", "Platform")]));
      const picker = await open();
      expect(await within(picker).findAllByText("owned by Platform")).toHaveLength(2);
      expect(within(picker).queryByRole("button", { name: /payments|tools/ })).not.toBeInTheDocument();
    });

    it("says owned by <team> (your team) for an on row the maintainer's own team owns", async () => {
      fakeApi(routes([row("acme/payments", "on", "Payments")]));
      const picker = await open();
      expect(await within(picker).findByText("owned by Payments (your team)")).toBeInTheDocument();
      expect(within(picker).queryByRole("button", { name: /payments/ })).not.toBeInTheDocument();
    });

    it("pre-selects the team named in ?team= for a maintainer of several teams", async () => {
      const second = { ...myTeams[1], id: 9, name: "Core", role: "maintainer" as const };
      fakeApi(routes([row("acme/payments", "off")], "", [myTeams[0], second, myTeams[1]]));
      renderApp("/repos?add=1&team=9");
      const picker = await screen.findByRole("region", { name: "Add repository" });
      await userEvent.selectOptions(await within(picker).findByLabelText("Instance"), "main");
      expect(await within(picker).findByLabelText("Owner team")).toHaveValue("9");
      expect(within(picker).getByRole("button", { name: "Onboard acme/payments" })).toBeEnabled();
    });

    it("asks for the team when the maintainer has more than one, with no None", async () => {
      const second = { ...myTeams[1], id: 9, name: "Core", role: "maintainer" as const };
      const fake = fakeApi(routes([row("acme/payments", "off")], "", [myTeams[0], second, myTeams[1]]));
      const picker = await open();
      const select = await within(picker).findByLabelText("Owner team");
      expect(within(select).getAllByRole("option").map((o) => o.textContent)).toEqual(["Choose a team", "Payments", "Core"]);
      expect(within(picker).getByRole("button", { name: "Onboard acme/payments" })).toBeDisabled();
      await userEvent.selectOptions(select, "Core");
      await userEvent.click(within(picker).getByRole("button", { name: "Onboard acme/payments" }));
      expect(fake.find("POST", "/api/repositories")[0].body).toEqual({ project: "acme/payments", instance: "main", ownerTeam: 9 });
    });

    it("replaces the select with the instance owner's line when the whole instance has an owner", async () => {
      const fake = fakeApi(routes([row("acme/payments", "off", "Payments")], "Payments"));
      const picker = await open();
      expect(await within(picker).findByText("Owner: Payments (all repositories of main)")).toBeInTheDocument();
      expect(within(picker).queryByLabelText("Owner team")).not.toBeInTheDocument();
      await userEvent.click(within(picker).getByRole("button", { name: "Onboard acme/payments" }));
      expect(fake.find("POST", "/api/repositories")[0].body).toEqual({ project: "acme/payments", instance: "main", ownerTeam: payments.id });
    });

    it("gives an admin Resume on a paused row, and no button on an on row", async () => {
      const fake = fakeApi({
        "GET /api/me": { body: ssoAdmin },
        "GET /api/repositories": { body: [repoWidgets] },
        "GET /api/admin/instances": { body: [instanceGitLab] },
        "GET /api/admin/instances/main/repositories?search=": {
          body: page([row("acme/payments", "paused"), row("acme/tools", "on"), row("acme/new", "off")]),
        },
        "PUT /api/admin/repositories/main/acme/payments": { body: repoDetail(repoPayments) },
      });
      renderApp("/repos?add=1");
      const picker = await screen.findByRole("region", { name: "Add repository" });
      await userEvent.selectOptions(await within(picker).findByLabelText("Instance"), "main");
      expect(await within(picker).findByRole("button", { name: "Switch on acme/new" })).toBeInTheDocument();
      expect(within(picker).queryByRole("button", { name: /acme\/tools/ })).not.toBeInTheDocument();
      await userEvent.click(within(picker).getByRole("button", { name: "Resume acme/payments" }));
      expect(fake.find("PUT", "/api/admin/repositories/main/acme/payments")[0].body).toEqual({ enabled: true });
    });
  });

  it("shows an admin owned by <team> on an on row that has an owner, and pre-selects ?team=", async () => {
    fakeApi({
      "GET /api/me": { body: ssoAdmin },
      "GET /api/repositories": { body: [repoWidgets] },
      "GET /api/admin/instances": { body: [instanceGitLab] },
      "GET /api/teams": { body: [teamPayments, teamEmpty] },
      "GET /api/admin/instances/main/repositories?search=": {
        body: {
          items: [{ instance: "main", project: "acme/tools", platform: "gitlab", state: "on", ownerTeam: "Payments" }],
          page: 1,
          hasMore: false,
          truncated: false,
          instanceOwner: "",
        },
      },
    });
    renderApp(`/repos?add=1&team=${teamEmpty.id}`);
    const picker = await screen.findByRole("region", { name: "Add repository" });
    await userEvent.selectOptions(await within(picker).findByLabelText("Instance"), "main");
    expect(await within(picker).findByText("owned by Payments")).toBeInTheDocument();
    expect(await within(picker).findByLabelText("Owner team")).toHaveValue(String(teamEmpty.id));
  });

  it("pauses a repository with the switch, with no confirm, and keeps it in the list", async () => {
    const confirm = vi.fn(() => true);
    vi.stubGlobal("confirm", confirm);
    const fake = fakeApi({
      "GET /api/me": { body: ssoAdmin },
      "GET /api/repositories": { body: [repoPayments, repoWidgets] },
      "GET /api/admin/instances": { body: [instanceGitLab] },
      "PUT /api/admin/repositories/main/acme/payments": {
        body: { ...repoDetail(repoPayments), repo: { ...repoPayments, on: false, paused: true, health: "paused" } },
      },
    });
    renderApp("/repos");
    const widgets = await screen.findByRole("switch", { name: "Review acme/widgets" });
    expect(widgets).toBeDisabled();
    const payments = screen.getByRole("switch", { name: "Review acme/payments" });
    expect(payments).toBeChecked();
    await userEvent.click(payments);
    await vi.waitFor(() => expect(fake.find("PUT", "/api/admin/repositories/main/acme/payments")).toHaveLength(1));
    const [put] = fake.find("PUT", "/api/admin/repositories/main/acme/payments");
    expect(put.body).toEqual({ enabled: false });
    expect(put.headers.get("X-CSRF-Token")).toBe("csrf-123");
    expect(confirm).not.toHaveBeenCalled();
    expect(fake.find("DELETE", "/api/repositories/main/acme/payments")).toHaveLength(0);
    // The list is read again.
    await vi.waitFor(() => expect(fake.find("GET", "/api/repositories").length).toBeGreaterThan(1));
  });

  it("resumes a paused repository with the switch", async () => {
    const fake = fakeApi({
      "GET /api/me": { body: ssoAdmin },
      "GET /api/repositories": { body: [repoToolsPaused] },
      "GET /api/admin/instances": { body: [instanceGitLab] },
      "PUT /api/admin/repositories/main/acme/tools": { body: repoDetail({ ...repoToolsPaused, on: true, paused: false, health: "ok" }) },
    });
    renderApp("/repos");
    const tools = await screen.findByRole("switch", { name: "Review acme/tools" });
    expect(tools).not.toBeChecked();
    expect(tools).toBeEnabled();
    await userEvent.click(tools);
    await vi.waitFor(() => expect(fake.find("PUT", "/api/admin/repositories/main/acme/tools")).toHaveLength(1));
    expect(fake.find("PUT", "/api/admin/repositories/main/acme/tools")[0].body).toEqual({ enabled: true });
  });

  it("shows Paused in the Health column", async () => {
    fakeApi({ "GET /api/me": { body: viewer }, "GET /api/repositories": { body: [repoToolsPaused] } });
    renderApp("/repos");
    const row = (await screen.findByRole("link", { name: "acme/tools" })).closest("tr") as HTMLElement;
    expect(within(row).getByText("Paused")).toHaveClass("pill", "grey");
    expect(within(row).queryByText("OK")).not.toBeInTheDocument();
    expect(within(row).queryByText("Needs attention")).not.toBeInTheDocument();
  });

  it("draws the switch disabled while the instance is being removed", async () => {
    fakeApi({
      "GET /api/me": { body: ssoAdmin },
      "GET /api/repositories": { body: [repoPayments, repoToolsPaused] },
      "GET /api/admin/instances": { body: [{ ...instanceGitLab, removing: true }] },
    });
    renderApp("/repos");
    for (const name of ["Review acme/payments", "Review acme/tools"]) {
      const sw = await screen.findByRole("switch", { name });
      await vi.waitFor(() => expect(sw).toBeDisabled());
    }
  });
});

describe("Repositories: links from the numbers", () => {
  it("links Needs attention to the Setup checks tab, and a failed count to those reviews", async () => {
    fakeApi({ "GET /api/me": { body: viewer }, "GET /api/repositories": { body: [repoPayments, repoWidgets] } });
    renderApp("/repos");
    expect(await screen.findByRole("link", { name: "Setup checks of acme/payments" })).toHaveAttribute("href", "/repos/main/acme/payments?tab=setup");
    expect(screen.getByRole("link", { name: "2 failed reviews of acme/payments" })).toHaveAttribute(
      "href",
      "/reviews?project=acme%2Fpayments&outcome=failed&days=7",
    );
    // No failure, no link.
    expect(screen.queryByRole("link", { name: /failed reviews of acme\/widgets/ })).not.toBeInTheDocument();
  });
});

describe("Repositories: owner, search, filter, sort", () => {
  const three = [repoPayments, repoWidgets, repoToolsPaused];
  const names = () =>
    screen
      .getAllByRole("link", { name: /^acme\// })
      .filter((l) => l.classList.contains("mr"))
      .map((l) => l.textContent);
  const location = () => screen.getByTestId("location");

  it("shows the owner team: a link when open, text when closed, a hint for an instance owner", async () => {
    const closed = { ...repoWidgets, ownerTeam: { id: 2, name: "ops", open: false, viaInstance: true } };
    fakeApi({ "GET /api/me": { body: viewer }, "GET /api/repositories": { body: [repoPayments, closed] } });
    renderApp("/repos");
    const payments = (await screen.findByRole("link", { name: "acme/payments" })).closest("tr") as HTMLElement;
    expect(within(payments).getByRole("link", { name: "payments" })).toHaveAttribute("href", "/teams/1");
    const widgets = screen.getByRole("link", { name: "acme/widgets" }).closest("tr") as HTMLElement;
    expect(within(widgets).queryByRole("link", { name: "ops" })).not.toBeInTheDocument();
    expect(within(widgets).getByText("ops").closest("td")).toHaveAttribute("title", "Owns every repository on main");
  });

  it("marks no owner as a warning link for an admin and as None for others", async () => {
    fakeApi({ "GET /api/me": { body: operator }, "GET /api/repositories": { body: three }, "GET /api/admin/instances": { body: [instanceGitLab] } });
    const first = renderApp("/repos");
    const widgets = (await screen.findByRole("link", { name: "acme/widgets" })).closest("tr") as HTMLElement;
    expect(within(widgets).getByRole("link", { name: "No owner" })).toHaveAttribute("href", "/admin/teams");
    expect(within(widgets).getByRole("link", { name: "No owner" })).toHaveAttribute("title", "Set the owner with the owner tick on a team's Repositories tab");
    first.unmount();
    fakeApi({ "GET /api/me": { body: viewer }, "GET /api/repositories": { body: three } });
    renderApp("/repos");
    const row = (await screen.findByRole("link", { name: "acme/widgets" })).closest("tr") as HTMLElement;
    expect(within(row).getByText("None")).toBeInTheDocument();
    expect(row).not.toHaveTextContent("⚠");
    expect(screen.queryByText(/has no owner|have no owner/)).not.toBeInTheDocument();
  });

  it("tells an admin how many active repositories have no owner and links to the No owner filter", async () => {
    fakeApi({ "GET /api/me": { body: operator }, "GET /api/repositories": { body: three }, "GET /api/admin/instances": { body: [instanceGitLab] } });
    renderApp("/repos");
    expect(await screen.findByText(/1 repository has no owner\./)).toBeInTheDocument();
    await userEvent.click(screen.getByRole("link", { name: "Show it" }));
    expect(location()).toHaveTextContent("team=none");
    expect(names()).toEqual(["acme/tools", "acme/widgets"]);
    expect(screen.queryByText(/has no owner\./)).not.toBeInTheDocument();
  });

  it("shows the No owner filter without the search text", async () => {
    fakeApi({ "GET /api/me": { body: operator }, "GET /api/repositories": { body: three }, "GET /api/admin/instances": { body: [instanceGitLab] } });
    renderApp("/repos?q=zzz");
    await userEvent.click(await screen.findByRole("link", { name: "Show it" }));
    expect(location()).toHaveTextContent("team=none");
    expect(location()).not.toHaveTextContent("q=");
    expect(names()).toEqual(["acme/tools", "acme/widgets"]);
  });

  it("searches the path, ignoring case, and survives odd characters", async () => {
    fakeApi({ "GET /api/me": { body: viewer }, "GET /api/repositories": { body: three } });
    renderApp("/repos");
    await screen.findByRole("link", { name: "acme/payments" });
    expect(screen.queryByText(/shown/)).not.toBeInTheDocument();
    await userEvent.type(screen.getByRole("searchbox", { name: "Search repositories" }), "PAY");
    await vi.waitFor(() => expect(names()).toEqual(["acme/payments"]));
    expect(screen.getByText("1 of 3 shown")).toBeInTheDocument();
    expect(location()).toHaveTextContent("q=PAY");
    await userEvent.clear(screen.getByRole("searchbox", { name: "Search repositories" }));
    await userEvent.type(screen.getByRole("searchbox", { name: "Search repositories" }), "(");
    expect(await screen.findByText("No repository matches.")).toBeInTheDocument();
  });

  it("clears an empty match back to all rows with no q or team", async () => {
    fakeApi({ "GET /api/me": { body: viewer }, "GET /api/repositories": { body: three } });
    renderApp("/repos?q=%25&team=1");
    expect(await screen.findByText("No repository matches.")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Clear" }));
    expect(names()).toHaveLength(3);
    expect(location()).not.toHaveTextContent("q=");
    expect(location()).not.toHaveTextContent("team=");
  });

  it("filters by team from the list itself", async () => {
    fakeApi({ "GET /api/me": { body: viewer }, "GET /api/repositories": { body: three } });
    renderApp("/repos");
    await screen.findByRole("link", { name: "acme/payments" });
    const select = screen.getByRole("combobox", { name: "Team" });
    expect(within(select).getAllByRole("option").map((o) => o.textContent)).toEqual(["All teams", "No owner", "payments"]);
    await userEvent.selectOptions(select, "payments");
    expect(location()).toHaveTextContent("team=1");
    expect(names()).toEqual(["acme/payments"]);
  });

  it("treats an unknown ?team= as All teams", async () => {
    fakeApi({ "GET /api/me": { body: viewer }, "GET /api/repositories": { body: three } });
    renderApp("/repos?team=99");
    await screen.findByRole("link", { name: "acme/payments" });
    expect(screen.getByRole("combobox", { name: "Team" })).toHaveDisplayValue("All teams");
    expect(names()).toHaveLength(3);
  });

  it("sorts by Reviews, biggest first, then reverses", async () => {
    fakeApi({ "GET /api/me": { body: viewer }, "GET /api/repositories": { body: three } });
    renderApp("/repos");
    await screen.findByRole("link", { name: "acme/payments" });
    await userEvent.click(screen.getByRole("button", { name: /^Reviews/ }));
    expect(names()).toEqual(["acme/payments", "acme/widgets", "acme/tools"]);
    await userEvent.click(screen.getByRole("button", { name: /^Reviews/ }));
    expect(names()).toEqual(["acme/tools", "acme/widgets", "acme/payments"]);
  });

  it("keeps the search when + Add repository opens", async () => {
    fakeApi({ "GET /api/me": { body: operator }, "GET /api/repositories": { body: three }, "GET /api/admin/instances": { body: [instanceGitLab] } });
    renderApp("/repos?q=pay");
    await userEvent.click(await screen.findByRole("button", { name: "+ Add repository" }));
    expect(location()).toHaveTextContent("q=pay");
    expect(location()).toHaveTextContent("add=1");
  });
});
