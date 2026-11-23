import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import type { ChainView, TeamModelsView } from "../api";
import { fakeApi } from "../test/fakeApi";
import {
  chainGlobal,
  chainTeam,
  choice,
  entryConfig,
  entrySonnet,
  entryTeamQwen,
  maintainer,
  myTeams,
  ssoAdmin,
  teamModelQwen,
  teamModelsPayments,
  viewer,
} from "../test/fixtures";
import { renderApp } from "../test/render";

afterEach(() => vi.unstubAllGlobals());

const routes = (me = maintainer, models: TeamModelsView = teamModelsPayments, chain: ChainView = chainTeam) => ({
  "GET /api/me": { body: me },
  "GET /api/me/teams": { body: myTeams },
  "GET /api/teams/2/models": { body: models },
  "GET /api/teams/2/chain": { body: chain },
});

// What a viewer of Payments is sent: no base URL, no edit, a read-only chain.
const asViewer: TeamModelsView = { ...teamModelsPayments, role: "viewer", canEdit: false, models: [{ ...teamModelQwen, baseURL: "" }] };
const readOnlyChain: ChainView = { ...chainTeam, readOnly: true, choices: [] };

const entry = (name: string) => screen.findByRole("group", { name });

describe("MyTeams", () => {
  it("lists the session's teams, its role on each and what each covers, linked to the team's page", async () => {
    fakeApi(routes());
    renderApp("/teams");
    expect(await screen.findByRole("heading", { name: "Teams" })).toBeInTheDocument();
    const payments = await screen.findByRole("region", { name: "Payments" });
    expect(payments).toHaveTextContent("Your role: maintainer");
    expect(payments).toHaveTextContent("acme/payments (owner)");
    expect(within(payments).getByRole("link", { name: "Payments" })).toHaveAttribute("href", "/teams/2");
    expect(screen.getByRole("region", { name: "Platform" })).toHaveTextContent("All repositories of main");
  });

  it("says so when the session is on no team", async () => {
    fakeApi({ ...routes(ssoAdmin), "GET /api/me/teams": { body: [] } });
    renderApp("/teams");
    expect(await screen.findByText("No team yet.")).toBeInTheDocument();
  });
});

describe("TeamModels", () => {
  it("shows a viewer the entries and their states with no button that changes them", async () => {
    fakeApi(routes(viewer, asViewer, readOnlyChain));
    renderApp("/teams/2");
    expect(await screen.findByRole("heading", { name: "Payments" })).toBeInTheDocument();
    const qwen = await entry("qwen3.5");
    expect(qwen).toHaveTextContent("openrouter / qwen/qwen3.5");
    expect(qwen).toHaveTextContent("Key set");
    expect(qwen).toHaveTextContent("In chains: team Payments, main/acme/payments");
    expect(qwen).not.toHaveTextContent("Base URL");
    expect(within(qwen).queryByRole("button")).not.toBeInTheDocument();
    expect(screen.queryByRole("form", { name: "Add a model" })).not.toBeInTheDocument();
    const chain = await screen.findByRole("region", { name: "Team chain" });
    expect(within(await within(chain).findByRole("list", { name: "Model chain" })).getAllByRole("listitem")).toHaveLength(2);
    expect(within(chain).getByText("qwen3.5")).toBeInTheDocument();
    expect(within(chain).queryByRole("button", { name: "Save chain" })).not.toBeInTheDocument();
  });

  it("lets a maintainer add an entry with a write-only key, and forgets the key", async () => {
    const fake = fakeApi({ ...routes(), "POST /api/teams/2/models": { status: 201, body: { ...teamModelQwen, id: "t-new", name: "Mistral" } } });
    renderApp("/teams/2");
    const form = await screen.findByRole("form", { name: "Add a model" });
    // Only an admin says which global entries teams may use.
    expect(within(form).queryByRole("switch")).not.toBeInTheDocument();
    await userEvent.type(within(form).getByLabelText("Name"), "Mistral");
    await userEvent.selectOptions(within(form).getByLabelText("Provider"), "mistral");
    await userEvent.type(within(form).getByLabelText("Model"), "mistral-large");
    const key = within(form).getByLabelText("API key");
    expect(key).toHaveAttribute("type", "password");
    expect(key).toHaveAttribute("autocomplete", "new-password");
    await userEvent.type(key, "sk-team-secret");
    await userEvent.click(within(form).getByRole("button", { name: "Add model" }));
    const [post] = fake.find("POST", "/api/teams/2/models");
    expect(post.headers.get("X-CSRF-Token")).toBe("csrf-maint");
    expect(post.body).toEqual({ name: "Mistral", provider: "mistral", model: "mistral-large", baseURL: "", apiKey: "sk-team-secret" });
    expect(await within(form).findByRole("status")).toHaveTextContent("Added Mistral.");
    await vi.waitFor(() => expect(within(form).getByLabelText("API key")).toHaveValue(""));
    expect(document.body.textContent).not.toContain("sk-team-secret");
  });

  it("changes an entry's name and base URL with a PUT to the team's route", async () => {
    const fake = fakeApi({ ...routes(), "PUT /api/teams/2/models/t-qwen": { body: teamModelQwen } });
    renderApp("/teams/2");
    await userEvent.click(within(await entry("qwen3.5")).getByRole("button", { name: "Edit qwen3.5" }));
    const form = screen.getByRole("form", { name: "Edit qwen3.5" });
    await userEvent.clear(within(form).getByLabelText("Name"));
    await userEvent.type(within(form).getByLabelText("Name"), "qwen main");
    await userEvent.click(within(form).getByRole("button", { name: "Save" }));
    expect(fake.find("PUT", "/api/teams/2/models/t-qwen")[0].body).toEqual({ name: "qwen main", baseURL: "https://router.example.com/v1" });
  });

  it("asks before a removal, naming the chains the entry is taken out of", async () => {
    const confirm = vi.fn(() => true);
    vi.stubGlobal("confirm", confirm);
    const fake = fakeApi({ ...routes(), "DELETE /api/teams/2/models/t-qwen": { status: 204 } });
    renderApp("/teams/2");
    await userEvent.click(within(await entry("qwen3.5")).getByRole("button", { name: "Remove qwen3.5" }));
    expect(confirm).toHaveBeenCalledWith(
      "Remove qwen3.5? Its key is deleted, and it is taken out of these chains: team Payments, main/acme/payments.",
    );
    expect(fake.find("DELETE", "/api/teams/2/models/t-qwen")[0].headers.get("X-CSRF-Token")).toBe("csrf-maint");
  });

  it("removes nothing when the confirmation is refused", async () => {
    vi.stubGlobal("confirm", vi.fn(() => false));
    const fake = fakeApi(routes());
    renderApp("/teams/2");
    await userEvent.click(within(await entry("qwen3.5")).getByRole("button", { name: "Remove qwen3.5" }));
    expect(fake.find("DELETE", "/api/teams/2/models/t-qwen")).toHaveLength(0);
  });

  it("offers the team's entries and the global entries teams may use in the picker, and saves the chain", async () => {
    const opusForTeams = { ...entrySonnet, id: "m-opus", name: "Claude Opus", model: "claude-opus" };
    const chain: ChainView = { ...chainTeam, choices: [choice(entryTeamQwen), choice(entrySonnet), choice(opusForTeams)] };
    const fake = fakeApi({ ...routes(maintainer, teamModelsPayments, chain), "PUT /api/teams/2/chain": { body: chain } });
    renderApp("/teams/2");
    const card = await screen.findByRole("region", { name: "Team chain" });
    const pick = await within(card).findByRole("combobox", { name: "Entry to add" });
    expect(within(pick).getAllByRole("option").map((o) => o.textContent)).toEqual(["Choose an entry to add", "Claude Opus (global)"]);
    await userEvent.selectOptions(pick, "Claude Opus (global)");
    await userEvent.click(within(card).getByRole("button", { name: "Add" }));
    await userEvent.click(within(card).getByRole("button", { name: "Move Claude Opus up" }));
    await userEvent.click(within(card).getByRole("button", { name: "Save chain" }));
    const [put] = fake.find("PUT", "/api/teams/2/chain");
    expect(put.headers.get("X-CSRF-Token")).toBe("csrf-maint");
    expect(put.body).toEqual({
      entries: [
        { source: "team", id: "t-qwen" },
        { source: "global", id: "m-opus" },
        { source: "global", id: "m-sonnet" },
      ],
    });
    expect(await within(card).findByRole("status")).toHaveTextContent("Saved.");
  });

  it("shows the global chain while the team has none, and starts a team chain from empty", async () => {
    // The server shows the global chain the team's repositories use.
    const none: ChainView = { ...chainGlobal, scope: "team", entries: [entryConfig, entrySonnet], choices: [choice(entryTeamQwen), choice(entrySonnet)] };
    const fake = fakeApi({ ...routes(maintainer, teamModelsPayments, none), "PUT /api/teams/2/chain": { body: chainTeam } });
    renderApp("/teams/2");
    const card = await screen.findByRole("region", { name: "Team chain" });
    expect(await within(card).findByText("This team has no chain of its own: its repositories use the global chain.")).toBeInTheDocument();
    expect(within(card).getByText("openai/gpt-5")).toBeInTheDocument();
    expect(within(card).queryByRole("button", { name: "Save chain" })).not.toBeInTheDocument();
    await userEvent.click(within(card).getByRole("button", { name: "Make a team chain" }));
    expect(within(card).getByText("No entries.")).toBeInTheDocument();
    await userEvent.selectOptions(within(card).getByRole("combobox", { name: "Entry to add" }), "qwen3.5 (team Payments)");
    await userEvent.click(within(card).getByRole("button", { name: "Add" }));
    await userEvent.click(within(card).getByRole("button", { name: "Save chain" }));
    expect(fake.find("PUT", "/api/teams/2/chain")[0].body).toEqual({ entries: [{ source: "team", id: "t-qwen" }] });
  });

  it("shows a banner while an entry has a mark, and clears it with the team's route", async () => {
    const until = "2026-11-21T14:00:00Z";
    const marked = { ...teamModelQwen, mark: { class: "billing" as const, until, setAt: until, live: true } };
    const fake = fakeApi({
      ...routes(maintainer, { ...teamModelsPayments, models: [marked] }),
      "DELETE /api/teams/2/models/t-qwen/mark": { status: 204 },
    });
    renderApp("/teams/2");
    expect(await screen.findByRole("note", { name: "Marked entries" })).toHaveTextContent("qwen3.5: out of credit until 2026-11-21 14:00 UTC");
    await userEvent.click(within(await entry("qwen3.5")).getByRole("button", { name: "Clear mark of qwen3.5" }));
    expect(fake.find("DELETE", "/api/teams/2/models/t-qwen/mark")[0].headers.get("X-CSRF-Token")).toBe("csrf-maint");
  });

  it("clears a team entry's mark from the chain with the team's route", async () => {
    const marked = { ...entryTeamQwen, position: 1, state: "exhausted" as const, class: "quota" as const, until: "2026-11-21T14:00:00Z", canClear: true };
    const fake = fakeApi({
      ...routes(maintainer, teamModelsPayments, { ...chainTeam, entries: [marked, { ...entrySonnet, position: 2 }] }),
      "DELETE /api/teams/2/models/t-qwen/mark": { status: 204 },
    });
    renderApp("/teams/2");
    const card = await screen.findByRole("region", { name: "Team chain" });
    await userEvent.click(await within(card).findByRole("button", { name: "Clear mark of qwen3.5" }));
    expect(fake.find("DELETE", "/api/teams/2/models/t-qwen/mark")).toHaveLength(1);
  });

  it("shows not found for a team the session doesn't see", async () => {
    fakeApi({ "GET /api/me": { body: viewer }, "GET /api/teams/9/models": { status: 404, body: { error: "not found" } } });
    renderApp("/teams/9");
    expect(await screen.findByRole("heading", { name: "Not found" })).toBeInTheDocument();
  });

  it("shows a model's name as text, never as HTML", async () => {
    fakeApi(routes(maintainer, { ...teamModelsPayments, models: [{ ...teamModelQwen, name: "<b>x</b>" }] }));
    renderApp("/teams/2");
    expect(await entry("<b>x</b>")).toBeInTheDocument();
    expect(document.querySelector("main b b")).toBeNull();
  });
});

describe("TeamModels: no chain B", () => {
  it("has no chain B card, and asks for no panel chain", async () => {
    const fake = fakeApi(routes());
    renderApp("/teams/2");
    const card = await screen.findByRole("region", { name: "Team chain" });
    expect(await within(card).findByText("Claude Sonnet")).toBeInTheDocument();
    expect(document.body).not.toHaveTextContent(/\bchain B\b/i);
    expect(fake.calls.filter((c) => c.url.includes("panel-chain"))).toHaveLength(0);
  });
});
