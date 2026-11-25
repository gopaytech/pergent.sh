import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import type { ChainView, CIJob, RepositoryDetailView } from "../api";
import { fakeApi } from "../test/fakeApi";
import {
  builtInRules,
  chainRepoCustom,
  chainRepoInherit,
  choice,
  entryConfig,
  entryOpus,
  entryTeamQwen,
  librarySkills,
  maintainer,
  reviewersNone,
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
  "GET /api/repositories/main/acme/payments/reviewers": { body: reviewersNone },
  "GET /api/repositories/main/acme/widgets/reviewers": { body: reviewersNone },
};

const teamsSection = () => screen.findByRole("region", { name: "Teams" });

describe("RepositorySettings: Teams", () => {
  it("shows an admin every team, ticked when it lists the repository, and the whole-instance ones as such", async () => {
    fakeApi(base);
    renderApp("/repos/main/acme/payments?tab=settings", { unfolded: true });
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
    renderApp("/repos/main/acme/payments?tab=settings", { unfolded: true });
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
    renderApp("/repos/main/acme/payments?tab=settings", { unfolded: true });
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
    renderApp("/repos/main/acme/payments?tab=settings", { unfolded: true });
    const box = await teamsSection();
    await userEvent.click(await within(box).findByRole("checkbox", { name: /Platform/ }));
    await userEvent.click(within(box).getByRole("button", { name: "Save teams" }));
    expect(await within(box).findByRole("alert")).toHaveTextContent("every team must be a team that exists");
  });

  it("is there for a repository the config sets up, whose settings are locked", async () => {
    fakeApi(base);
    renderApp("/repos/main/acme/widgets?tab=settings", { unfolded: true });
    const box = await teamsSection();
    expect(await within(box).findByRole("checkbox", { name: /Payments/ })).toBeEnabled();
    expect(screen.getByText(/listed in pergent.sh's config/)).toBeInTheDocument();
  });

  it("points to Teams when there is none", async () => {
    fakeApi({ ...base, "GET /api/repositories/main/acme/payments/teams": { body: { teams: [] } } });
    renderApp("/repos/main/acme/payments?tab=settings", { unfolded: true });
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
    renderApp("/repos/main/acme/payments?tab=settings", { unfolded: true });
    await screen.findByRole("switch", { name: "Approve clean reviews" });
    expect(screen.queryByRole("region", { name: "Teams" })).not.toBeInTheDocument();
    expect(fake.calls.filter((c) => c.url.endsWith("/teams"))).toHaveLength(0);
  });
});

describe("RepositorySettings: reviewer 1, the model chain", () => {
  const payments = "/api/repositories/main/acme/payments";
  const chainPath = `${payments}/chain`;
  const routes = (detail: RepositoryDetailView, me = ssoAdmin, chain: ChainView = chainRepoInherit) => ({
    ...base,
    "GET /api/me": { body: me },
    [`GET ${payments}`]: { body: detail },
    [`PUT ${payments}/settings`]: { body: detail },
    [`GET ${chainPath}`]: { body: chain },
    [`GET ${payments}/reviewers`]: { body: reviewersNone },
  });
  // card is reviewer 1 in the Reviewers card.
  const card = async () => within(await screen.findByRole("region", { name: "Reviewers" })).findByRole("group", { name: "Reviewer 1" });
  // edit opens reviewer 1's editor.
  const edit = async (box: HTMLElement) => userEvent.click(await within(box).findByRole("button", { name: "Edit reviewer 1" }));
  const names = (box: HTMLElement) =>
    within(within(box).getByRole("list", { name: "Reviewer 1 chain" }))
      .getAllByRole("listitem")
      .map((r) => r.querySelector("b")?.textContent);

  it("has no model menu any more, and sends no model in the settings", async () => {
    const fake = fakeApi(routes(repoDetail(repoPayments)));
    renderApp("/repos/main/acme/payments?tab=settings", { unfolded: true });
    await userEvent.click(await screen.findByRole("switch", { name: "Quiet reviews" }));
    expect(screen.queryByRole("combobox", { name: "Model" })).not.toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(fake.find("PUT", `${payments}/settings`)[0].body).toEqual({ quietReviews: true });
  });

  it("shows the chain the repository inherits and where it comes from", async () => {
    fakeApi(routes(repoDetail(repoPayments)));
    renderApp("/repos/main/acme/payments?tab=settings", { unfolded: true });
    const box = await card();
    expect(await within(box).findByText("Inherited from the chain of its owner team, Payments.")).toBeInTheDocument();
    expect(names(box)).toEqual(["qwen3.5", "Claude Sonnet"]);
    expect(within(box).queryByRole("button", { name: "Save chain" })).not.toBeInTheDocument();
    expect(within(box).getByRole("button", { name: "Edit reviewer 1" })).toBeInTheDocument();
  });

  it("says when the repository inherits the global chain", async () => {
    fakeApi(routes(repoDetail(repoPayments), ssoAdmin, { ...chainRepoInherit, source: "global", team: "" }));
    renderApp("/repos/main/acme/payments?tab=settings", { unfolded: true });
    expect(await within(await card()).findByText("Inherited from the global chain.")).toBeInTheDocument();
  });

  it("gives the repository its own chain, starting from the one in effect, with a PUT of the entries", async () => {
    const fake = fakeApi({ ...routes(repoDetail(repoPayments)), [`PUT ${chainPath}`]: { body: chainRepoCustom } });
    renderApp("/repos/main/acme/payments?tab=settings", { unfolded: true });
    const box = await card();
    await edit(box);
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
    renderApp("/repos/main/acme/payments?tab=settings", { unfolded: true });
    const box = await card();
    expect(await within(box).findByText("This repository has its own chain.")).toBeInTheDocument();
    await edit(box);
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
    renderApp("/repos/main/acme/payments?tab=settings", { unfolded: true });
    const box = await card();
    await edit(box);
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
    renderApp("/repos/main/acme/payments?tab=settings", { unfolded: true });
    const box = await card();
    expect(await within(box).findByText("An admin saved this chain.")).toBeInTheDocument();
    await edit(box);
    expect(within(box).getByText("You can't keep this entry here. Remove it to save.")).toBeInTheDocument();
    expect(within(box).getByRole("button", { name: "Save chain" })).toBeDisabled();
  });

  it("shows a maintainer of another team the chain with no button that changes it", async () => {
    // The server says read-only: the session doesn't maintain the owner team.
    fakeApi({
      ...routes(repoDetail(repoPayments, "maintainer"), viewer, { ...chainRepoCustom, readOnly: true, choices: [] }),
      [`GET ${payments}/reviewers`]: { body: { ...reviewersNone, readOnly: true } },
    });
    renderApp("/repos/main/acme/payments?tab=settings", { unfolded: true });
    const box = await card();
    expect(await within(box).findByText("This repository has its own chain.")).toBeInTheDocument();
    // The only button is the card's fold.
    const changes = (el: HTMLElement) => within(el).queryAllByRole("button").filter((b) => !b.hasAttribute("data-fold"));
    expect(changes(box)).toHaveLength(0);
    const reviewers = screen.getByRole("region", { name: "Reviewers" });
    expect(await within(reviewers).findByText("Only admins and maintainers of the repository's owner team change its reviewers.")).toBeInTheDocument();
    expect(changes(reviewers)).toHaveLength(0);
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
    renderApp("/repos/main/acme/payments?tab=settings", { unfolded: true });
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
    renderApp("/repos/main/acme/payments?tab=settings", { unfolded: true });
    await userEvent.click(await within(await card()).findByRole("button", { name: "Clear mark of qwen3.5" }));
    await vi.waitFor(() => expect(fake.find("DELETE", "/api/teams/2/models/t-qwen/mark")).toHaveLength(1));
  });

  it("says in a maintainer's reset confirmation that the chain stays", async () => {
    fakeApi(routes(repoDetail(repoPayments, "maintainer"), viewer));
    const confirm = vi.spyOn(window, "confirm").mockReturnValue(false);
    renderApp("/repos/main/acme/payments?tab=settings", { unfolded: true });
    await userEvent.click(await screen.findByRole("button", { name: "Reset to defaults" }));
    expect(confirm).toHaveBeenCalledWith(expect.stringContaining("who is trusted, the token quota and the status settings"));
    expect(confirm).toHaveBeenCalledWith(expect.stringContaining("The model chain stays as it is."));
    confirm.mockRestore();
  });
});

describe("RepositorySettings: the status settings", () => {
  const payments = "/api/repositories/main/acme/payments";
  const routes = (detail: RepositoryDetailView, me = ssoAdmin) => ({
    ...base,
    "GET /api/me": { body: me },
    [`GET ${payments}`]: { body: detail },
    [`PUT ${payments}/settings`]: { body: detail },
  });
  const failure = () => screen.findByRole("combobox", { name: "Status when a review can't run" });
  const blocking = () => screen.findByRole("combobox", { name: "Status when a review finds blocking issues" });

  it("lets an admin set the blocking status, and sends only that", async () => {
    const fake = fakeApi(routes(repoDetail(repoPayments)));
    renderApp("/repos/main/acme/payments?tab=settings", { unfolded: true });
    expect(await blocking()).toHaveValue("failed");
    expect(await blocking()).toBeEnabled();
    expect(await failure()).toBeEnabled();
    expect(screen.queryByText("Only an admin changes this.")).not.toBeInTheDocument();
    expect(screen.queryByText("Not used: the blocking status is neutral")).not.toBeInTheDocument();
    await userEvent.selectOptions(await blocking(), "neutral");
    // The failure status is not used now, but stays changeable.
    expect(screen.getByText("Not used: the blocking status is neutral")).toBeInTheDocument();
    expect(await failure()).toBeEnabled();
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(fake.find("PUT", `${payments}/settings`)[0].body).toEqual({ blockingStatus: "neutral" });
  });

  it("shows a maintainer both statuses read-only, and never sends them", async () => {
    const detail = repoDetail(repoPayments, "maintainer");
    detail.settings.blockingStatus = { value: "neutral", overridden: true };
    const fake = fakeApi(routes(detail, viewer));
    renderApp("/repos/main/acme/payments?tab=settings", { unfolded: true });
    expect(await blocking()).toHaveValue("neutral");
    expect(await blocking()).toBeDisabled();
    expect(await failure()).toBeDisabled();
    expect(screen.getAllByText("Only an admin changes this.")).toHaveLength(2);
    expect(screen.getByText("Not used: the blocking status is neutral")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("switch", { name: "Approve clean reviews" }));
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(fake.find("PUT", `${payments}/settings`)[0].body).toEqual({ approve: false });
  });

  it("says in a maintainer's reset confirmation that the status settings stay", async () => {
    fakeApi(routes(repoDetail(repoPayments, "maintainer"), viewer));
    const confirm = vi.spyOn(window, "confirm").mockReturnValue(false);
    renderApp("/repos/main/acme/payments?tab=settings", { unfolded: true });
    await userEvent.click(await screen.findByRole("button", { name: "Reset to defaults" }));
    expect(confirm).toHaveBeenCalledWith(expect.stringContaining("the status settings"));
    confirm.mockRestore();
  });
});

describe("RepositorySettings: Review deleted and renamed files", () => {
  const help =
    "Reviews the lines a change deletes, and checks what still uses deleted files and renamed files' old paths. Costs more tokens. Needs pergent v0.10.0 or newer; an older image ignores it.";

  it("shows the switch after Quiet reviews, off, with its help text", async () => {
    fakeApi(base);
    renderApp("/repos/main/acme/payments?tab=settings", { unfolded: true });
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
    renderApp("/repos/main/acme/payments?tab=settings", { unfolded: true });
    await userEvent.click(await screen.findByRole("switch", { name: "Review deleted and renamed files" }));
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(fake.find("PUT", "/api/repositories/main/acme/payments/settings")[0].body).toEqual({ checkDeleted: true });
  });

  it("shows it on when the repository sets it", async () => {
    const own = repoDetail(repoPayments);
    own.settings.checkDeleted = { value: true, overridden: true };
    fakeApi({ ...base, "GET /api/repositories/main/acme/payments": { body: own } });
    renderApp("/repos/main/acme/payments?tab=settings", { unfolded: true });
    expect(await screen.findByRole("switch", { name: "Review deleted and renamed files" })).toBeChecked();
  });
});

describe("RepositorySettings: unsaved changes", () => {
  const toggle = () => screen.findByRole("switch", { name: "Quiet reviews" });

  it("offers Save in a bar only while something changed, and Discard puts the draft back", async () => {
    fakeApi(base);
    renderApp("/repos/main/acme/payments?tab=settings", { unfolded: true });
    const quiet = await toggle();
    expect(screen.queryByRole("region", { name: "Unsaved changes" })).not.toBeInTheDocument();
    await userEvent.click(quiet);
    const bar = screen.getByRole("region", { name: "Unsaved changes" });
    expect(bar).toHaveTextContent("You have unsaved changes to the settings above.");
    await userEvent.click(within(bar).getByRole("button", { name: "Discard" }));
    expect(quiet).not.toBeChecked();
    expect(screen.queryByRole("region", { name: "Unsaved changes" })).not.toBeInTheDocument();
  });

  it("asks before another tab drops the changes, and keeps them when the person stays", async () => {
    fakeApi(base);
    const ask = vi.spyOn(window, "confirm").mockReturnValueOnce(false).mockReturnValueOnce(true);
    renderApp("/repos/main/acme/payments?tab=settings", { unfolded: true });
    await userEvent.click(await toggle());
    await userEvent.click(screen.getByRole("tab", { name: "Reviews" }));
    expect(ask).toHaveBeenCalledWith("You have unsaved changes. Leave and lose them?");
    expect(screen.getByRole("tab", { name: "Settings" })).toHaveAttribute("aria-selected", "true");
    expect(await toggle()).toBeChecked();
    await userEvent.click(screen.getByRole("tab", { name: "Reviews" }));
    await waitFor(() => expect(screen.getByRole("tab", { name: "Reviews" })).toHaveAttribute("aria-selected", "true"));
    ask.mockRestore();
  });

  it("keeps the draft when the settings are read again", async () => {
    fakeApi(base);
    const { client } = renderApp("/repos/main/acme/payments?tab=settings", { unfolded: true });
    await userEvent.click(await toggle());
    await client.refetchQueries({ queryKey: ["repo"] });
    expect(await toggle()).toBeChecked();
    expect(screen.getByRole("region", { name: "Unsaved changes" })).toBeInTheDocument();
  });

  it("takes the person to a field the save refuses", async () => {
    const fake = fakeApi(base);
    renderApp("/repos/main/acme/payments?tab=settings", { unfolded: true });
    const steps = await screen.findByRole("textbox", { name: "Max steps" });
    await userEvent.clear(steps);
    await userEvent.type(steps, "999");
    await userEvent.click(within(screen.getByRole("region", { name: "Unsaved changes" })).getByRole("button", { name: "Save" }));
    expect(within(screen.getByRole("region", { name: "Unsaved changes" })).getByRole("alert")).toHaveTextContent(/Max steps/);
    expect(steps).toHaveFocus();
    expect(fake.find("PUT", "/api/repositories/main/acme/payments/settings")).toHaveLength(0);
  });
});

describe("RepositorySettings: folded cards", () => {
  // fold is the button in a card's title that folds and opens it.
  const fold = (card: string) => within(screen.getByRole("region", { name: card })).getAllByRole("button")[0];

  it("opens Reviews and folds every other card, each with a summary", async () => {
    fakeApi(base);
    renderApp("/repos/main/acme/payments?tab=settings");
    expect(await screen.findByRole("switch", { name: "Approve clean reviews" })).toBeVisible();
    expect(fold("Review settings")).toHaveAttribute("aria-expanded", "true");
    expect(fold("Review settings")).toHaveTextContent(/blocking issues: (failed|neutral)/);
    for (const card of ["CI jobs", "Comment layout", "Skills used for this repository", "Built-in rules", "Review Job"]) {
      expect(fold(card)).toHaveAttribute("aria-expanded", "false");
    }
    expect(fold("CI jobs")).toHaveTextContent("none chosen");
    expect(fold("Built-in rules")).toHaveTextContent("none enabled");
    expect(fold("Review Job")).toHaveTextContent(/timeout \d+s · \d+ steps/);
    expect(await screen.findByRole("button", { name: /^Reviewers/ })).toHaveAttribute("aria-expanded", "false");
    // A folded card's fields are hidden until it is opened.
    expect(screen.queryByRole("textbox", { name: "Max steps" })).not.toBeInTheDocument();
    await userEvent.click(fold("Review Job"));
    expect(screen.getByRole("textbox", { name: "Max steps" })).toBeVisible();
  });

  it("keeps an edit in a card that is folded again, and opens the card when the save refuses its field", async () => {
    const fake = fakeApi(base);
    renderApp("/repos/main/acme/payments?tab=settings");
    await screen.findByRole("switch", { name: "Approve clean reviews" });
    await userEvent.click(fold("Review Job"));
    const steps = screen.getByRole("textbox", { name: "Max steps" });
    await userEvent.clear(steps);
    await userEvent.type(steps, "999");
    expect(fold("Review Job")).toHaveTextContent("999 steps");
    await userEvent.click(fold("Review Job"));
    expect(screen.queryByRole("textbox", { name: "Max steps" })).not.toBeInTheDocument();
    await userEvent.click(within(screen.getByRole("region", { name: "Unsaved changes" })).getByRole("button", { name: "Save" }));
    await waitFor(() => expect(steps).toHaveFocus());
    expect(fold("Review Job")).toHaveAttribute("aria-expanded", "true");
    expect(steps).toHaveValue("999");
    expect(fake.find("PUT", "/api/repositories/main/acme/payments/settings")).toHaveLength(0);
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
    renderApp("/repos/main/acme/payments?tab=settings", { unfolded: true });
    const box = await card();
    expect(within(box).getByRole("textbox", { name: "How long to wait for CI" })).toHaveValue("30m");
    expect(within(box).getByText("No CI job yet.")).toBeInTheDocument();
    expect(within(box).getByText("default")).toBeInTheDocument();
  });

  it("suggests the names the server lists, and takes free text", async () => {
    const fake = fakeApi(routes(repoDetail(repoPayments)));
    renderApp("/repos/main/acme/payments?tab=settings", { unfolded: true });
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
    renderApp("/repos/main/acme/payments?tab=settings", { unfolded: true });
    const box = await card();
    await userEvent.click(within(box).getByRole("button", { name: "Add a job" }));
    await userEvent.type(within(box).getByRole("combobox", { name: "Job name" }), "deploy-docs");
    expect(within(box).queryByText("Not seen in the last 10 runs. Check the name.")).not.toBeInTheDocument();
  });

  it("sends ciJobs, and only that, when a job is added", async () => {
    const fake = fakeApi(routes(repoDetail(repoPayments)));
    renderApp("/repos/main/acme/payments?tab=settings", { unfolded: true });
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
    renderApp("/repos/hub/acme/gadgets?tab=settings", { unfolded: true });
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
    renderApp("/repos/main/acme/payments?tab=settings", { unfolded: true });
    const box = await card();
    expect(within(box).getByText("overridden here")).toBeInTheDocument();
    expect(within(box).getByRole("textbox", { name: "How long to wait for CI" })).toHaveValue("45m");
    // Nothing changed: there is nothing to save, so no Save is offered.
    expect(screen.queryByRole("button", { name: "Save" })).not.toBeInTheDocument();
    expect(screen.getByText("All changes are saved.")).toBeInTheDocument();
    expect(fake.find("PUT", settingsPath)).toHaveLength(0);
    await userEvent.click(within(box).getByRole("button", { name: "Remove test" }));
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(fake.find("PUT", settingsPath)[0].body).toEqual({ ciJobs: null });
  });

  it("shows the wait of a repository that has no job, and sends a changed wait with its jobs only", async () => {
    const own = withJobs(repoDetail(repoPayments), "45m", [{ name: "test", read: "status", blocksApproval: false }]);
    const fake = fakeApi(routes(own));
    renderApp("/repos/main/acme/payments?tab=settings", { unfolded: true });
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
    renderApp("/repos/main/acme/widgets?tab=settings", { unfolded: true });
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
    renderApp("/repos/main/acme/payments?tab=settings", { unfolded: true });
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
    renderApp("/repos/main/acme/payments?tab=settings", { unfolded: true });
    const card = await screen.findByRole("region", { name: "Rule sets used for this repository" });
    await userEvent.click(await within(card).findByRole("button", { name: "Remove go-api" }));
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(await screen.findByText("Choose at least one rule set, or use the default rule sets.")).toBeInTheDocument();
    await userEvent.click(within(card).getByRole("radio", { name: /Use the default rule sets/ }));
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(fake.find("PUT", settingsPath)[0].body).toEqual({ ruleSets: null });
  });

  it("sends only the enabled built-in rules, in pergent's order", async () => {
    const fake = fakeApi({
      ...base,
      "GET /api/rule-sets": { body: ruleSets },
      "GET /api/built-in-rules": { body: builtInRules },
      [`PUT ${settingsPath}`]: { body: repoDetail(repoPayments) },
    });
    renderApp("/repos/main/acme/payments?tab=settings", { unfolded: true });
    // None by default.
    const card = await screen.findByRole("region", { name: "Built-in rules" });
    expect(await within(card).findByText("0 of 2 enabled")).toBeInTheDocument();
    expect(within(card).queryByRole("switch")).not.toBeInTheDocument();
    await userEvent.click(within(card).getByRole("checkbox", { name: "Built-in rule ts_js" }));
    await userEvent.click(within(card).getByRole("checkbox", { name: "Built-in rule github_workflows" }));
    expect(within(card).getByText("2 of 2 enabled")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(fake.find("PUT", settingsPath)[0].body).toEqual({ builtInRules: ["github_workflows", "ts_js"] });
  });

  it("sends null when Select none clears the enabled built-in rules", async () => {
    const detail = repoDetail(repoPayments);
    detail.settings.builtInRules = { value: ["ts_js"], overridden: true };
    const fake = fakeApi({
      ...base,
      [`GET /api/repositories/main/acme/payments`]: { body: detail },
      "GET /api/built-in-rules": { body: builtInRules },
      [`PUT ${settingsPath}`]: { body: repoDetail(repoPayments) },
    });
    renderApp("/repos/main/acme/payments?tab=settings", { unfolded: true });
    const card = await screen.findByRole("region", { name: "Built-in rules" });
    expect(await within(card).findByRole("checkbox", { name: "Built-in rule ts_js" })).toBeChecked();
    expect(within(card).getByText("overridden here")).toBeInTheDocument();
    await userEvent.click(within(card).getByRole("button", { name: "Select none" }));
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(fake.find("PUT", settingsPath)[0].body).toEqual({ builtInRules: null });
  });

  it("says when reviews can't load the enabled built-in rules yet", async () => {
    const detail = repoDetail(repoPayments);
    detail.builtInRulesOff = "Reviews get no built-in rules for now: no review image can load only the enabled built-in rules yet.";
    fakeApi({ ...base, [`GET /api/repositories/main/acme/payments`]: { body: detail }, "GET /api/built-in-rules": { body: builtInRules } });
    renderApp("/repos/main/acme/payments?tab=settings", { unfolded: true });
    const card = await screen.findByRole("region", { name: "Built-in rules" });
    const note = await within(card).findByText(/Reviews get no built-in rules for now/);
    // Only a note while none is enabled; a problem once one is.
    expect(note).not.toHaveClass("problem");
    await userEvent.click(await within(card).findByRole("checkbox", { name: "Built-in rule ts_js" }));
    expect(within(card).getByText(/Reviews get no built-in rules for now/)).toHaveClass("problem");
  });
});
