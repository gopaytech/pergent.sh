import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import type { TeamUsageView, UsageTokensView } from "../api";
import { fakeApi } from "../test/fakeApi";
import { maintainer, myTeams, teamPagePayments } from "../test/fixtures";
import { renderApp } from "../test/render";

afterEach(() => vi.unstubAllGlobals());

const tok = (inn: number, out: number, apart = { cacheRead: 0, cacheWrite: 0, reasoning: 0, unknown: 0 }): UsageTokensView => ({ in: inn, out, ...apart });

const usage: TeamUsageView = {
  days: 30,
  tokens: tok(1_000_000, 200_000, { cacheRead: 500_000, cacheWrite: 20_000, reasoning: 7_000, unknown: 0 }),
  reviews: 12,
  daily: [
    { day: "2026-11-06", tokens: tok(600_000, 150_000) },
    { day: "2026-11-07", tokens: tok(400_000, 50_000) },
  ],
  repositories: [
    { instance: "main", project: "acme/payments", tokens: tok(900_000, 100_000), reviews: 9 },
    { instance: "main", project: "acme/widgets", tokens: tok(100_000, 100_000), reviews: 3 },
  ],
  models: [
    { provider: "anthropic", name: "sonnet", tokens: tok(800_000, 150_000), runs: 20, failed: 0 },
    { provider: "", name: "local", tokens: tok(200_000, 50_000), runs: 5, failed: 0 },
  ],
  partialRange: false,
};

const path = "/api/teams/2/usage?days=30";
const routes = (body: TeamUsageView = usage) => ({
  "GET /api/me": { body: maintainer },
  "GET /api/me/teams": { body: myTeams },
  "GET /api/teams/2": { body: teamPagePayments },
  [`GET ${path}`]: { body },
});

describe("Team Usage tab", () => {
  it("shows totals, days, repositories and models busiest first", async () => {
    fakeApi(routes());
    renderApp("/teams/2?tab=usage");
    const totals = await screen.findByRole("region", { name: "Totals" });
    // Input plus output, not the cache: 1,000k + 200k.
    expect(within(totals).getByText("1.2M")).toBeInTheDocument();
    expect(within(totals).getByText("12")).toBeInTheDocument();
    expect(screen.getByText(/cache read 500k · cache write 20k · reasoning 7k/)).toBeInTheDocument();
    const repos = screen.getByRole("region", { name: "Repositories" });
    const rows = within(repos).getAllByRole("row").slice(1);
    expect(rows.map((r) => within(r).getAllByRole("cell")[0].textContent)).toEqual(["main/acme/payments", "main/acme/widgets"]);
    expect(within(rows[0]).getAllByRole("cell").slice(0, 5).map((c) => c.textContent)).toEqual(["main/acme/payments", "9", "900k", "100k", "1M"]);
    expect(within(repos).getByRole("link", { name: "main/acme/payments" })).toHaveAttribute("href", "/repos/main/acme/payments");
    const models = screen.getByRole("region", { name: "Models" });
    expect(within(models).getAllByRole("row").slice(1).map((r) => within(r).getAllByRole("cell")[0].textContent)).toEqual(["sonnet", "local"]);
    const daily = screen.getByRole("region", { name: "Daily" });
    expect(within(daily).getByText("2026-11-06")).toBeInTheDocument();
    expect(within(daily).getByText("2026-11-07")).toBeInTheDocument();
  });

  it("the period select asks for days=7", async () => {
    const fake = fakeApi({ ...routes(), "GET /api/teams/2/usage?days=7": { body: { ...usage, days: 7 } } });
    renderApp("/teams/2?tab=usage");
    await screen.findByRole("region", { name: "Totals" });
    await userEvent.selectOptions(screen.getByRole("combobox", { name: "Period" }), "7");
    await vi.waitFor(() => expect(fake.find("GET", "/api/teams/2/usage?days=7")).toHaveLength(1));
  });

  it("partialRange shows Counts reviews since v0.19.0, and false does not", async () => {
    fakeApi(routes({ ...usage, partialRange: true }));
    const first = renderApp("/teams/2?tab=usage");
    expect(await screen.findByText("Counts reviews since v0.19.0")).toBeInTheDocument();
    first.unmount();
    fakeApi(routes());
    renderApp("/teams/2?tab=usage");
    await screen.findByRole("region", { name: "Totals" });
    expect(screen.queryByText(/since v0\.19\.0/)).not.toBeInTheDocument();
  });

  it("no usage shows an empty row", async () => {
    fakeApi(routes({ ...usage, tokens: tok(0, 0), reviews: 0, daily: [], repositories: [], models: [] }));
    renderApp("/teams/2?tab=usage");
    for (const name of ["Repositories", "Models", "Daily"]) {
      const section = await screen.findByRole("region", { name });
      expect(within(section).getByText("No usage in this period.")).toBeInTheDocument();
    }
  });
});
