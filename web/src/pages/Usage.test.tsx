import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { fakeApi } from "../test/fakeApi";
import { fiveParts, operator, usageOverview, usageRepository, viewer } from "../test/fixtures";
import { renderApp } from "../test/render";

afterEach(() => vi.unstubAllGlobals());

const overviewPath = "/api/admin/usage?days=30";
const repoPath = "/api/repositories/main/acme/payments/usage?days=30";

describe("Usage", () => {
  it("lists the repositories in the order the API gives, with the totals", async () => {
    fakeApi({ "GET /api/me": { body: operator }, [`GET ${overviewPath}`]: { body: usageOverview } });
    renderApp("/admin/usage");
    expect(await screen.findByRole("heading", { name: "Usage" })).toBeInTheDocument();
    const repos = (await screen.findByRole("region", { name: "Repositories" })) as HTMLElement;
    const rows = within(repos).getAllByRole("row").slice(1);
    expect(rows.map((r) => within(r).getAllByRole("cell")[0].textContent)).toEqual(["main/acme/payments", "main/acme/widgets", "main/acme/oldremovedCSV"]);
    expect(within(rows[0]).getAllByRole("cell").slice(0, 6).map((c) => c.textContent)).toEqual(["main/acme/payments", "30", "900k", "200k", "1.1M", "0%"]);
  });

  it("shows prompt, output, total and cached, with what each holds", async () => {
    fakeApi({ "GET /api/me": { body: operator }, [`GET ${overviewPath}`]: { body: { ...usageOverview, tokens: fiveParts, reviews: 7 } } });
    renderApp("/admin/usage");
    const totals = await screen.findByRole("region", { name: "Totals" });
    const cards = [...totals.querySelectorAll(".stat")].map((c) => c.textContent);
    expect(cards).toEqual(["Tokens604.5kall token kinds added", "Prompt600k87% from cache", "Output4.5kincl. reasoning", "Reviews7with usage recorded"]);
    expect(screen.getByText("Prompt: input · cache read · cache write")).toBeInTheDocument();
    expect(screen.getByText("Output: output · reasoning")).toBeInTheDocument();
    expect(screen.getByText(/Totals count all tokens: input, cache read, cache write, output and reasoning\./)).toBeInTheDocument();
    expect(screen.queryByText(/Reported apart/)).toBeNull();
    expect(screen.queryByRole("columnheader", { name: "Cache and reasoning" })).toBeNull();
  });

  it("orders the heads Reviews, Prompt, Output, Tokens, Cached, and names the parts on the total", async () => {
    fakeApi({ "GET /api/me": { body: operator }, [`GET ${overviewPath}`]: { body: usageOverview } });
    renderApp("/admin/usage");
    const repos = (await screen.findByRole("region", { name: "Repositories" })) as HTMLElement;
    expect(within(repos).getAllByRole("columnheader").map((h) => h.textContent).slice(1, 6)).toEqual(["Reviews", "Prompt", "Output", "Tokens", "Cached"]);
    // Tokens in the first row: 900k + 200k, no cache: input 900k, output 200k.
    const parts = within(repos).getAllByLabelText("input 900k · cache read 0 · cache write 0 · output 200k · reasoning 0")[0];
    expect(parts).toHaveAttribute("tabindex", "0");
    expect(parts).toHaveAttribute("title", "input 900k · cache read 0 · cache write 0 · output 200k · reasoning 0");
    expect(parts).toHaveTextContent("1.1M");
  });

  it("names Prompt and Output on a daily bar", async () => {
    fakeApi({ "GET /api/me": { body: operator }, [`GET ${overviewPath}`]: { body: usageOverview } });
    renderApp("/admin/usage");
    const daily = await screen.findByRole("region", { name: "Daily" });
    expect(within(daily).getByLabelText("2026-11-05 share of the busiest day")).toHaveAttribute("title", "Prompt 200k · Output 50k");
  });

  it("splits each repository's tokens by global and team keys", async () => {
    fakeApi({ "GET /api/me": { body: operator }, [`GET ${overviewPath}`]: { body: usageOverview } });
    renderApp("/admin/usage");
    const repos = (await screen.findByRole("region", { name: "Repositories" })) as HTMLElement;
    const heads = within(repos).getAllByRole("columnheader").map((h) => h.textContent);
    expect(heads.slice(-2)).toEqual(["On global keys", "On team keys"]);
    const rows = within(repos).getAllByRole("row").slice(1);
    expect(within(rows[0]).getAllByRole("cell").slice(-2).map((c) => c.textContent)).toEqual(["800k", "300k"]);
    expect(within(rows[1]).getAllByRole("cell").slice(-2).map((c) => c.textContent)).toEqual(["280k", "0"]);
    // An API client has no team: its table has no split.
    const clients = screen.getByRole("region", { name: "API clients" });
    expect(within(clients).queryByRole("columnheader", { name: "On team keys" })).toBeNull();
  });

  it("lists the API clients with their tenants under each", async () => {
    fakeApi({ "GET /api/me": { body: operator }, [`GET ${overviewPath}`]: { body: usageOverview } });
    renderApp("/admin/usage");
    const clients = (await screen.findByRole("region", { name: "API clients" })) as HTMLElement;
    const rows = within(clients).getAllByRole("row").slice(1);
    // A client's row, then one row per tenant under it; "" is no tenant.
    expect(rows.map((r) => within(r).getAllByRole("cell")[0].textContent)).toEqual([
      "fastgateway",
      "acme",
      "<img src=x onerror=alert(1)>",
      "(no tenant)",
      "docs-bot",
      "beta",
      "and 7 more tenants",
    ]);
    expect(within(rows[0]).getAllByRole("cell").slice(0, 6).map((c) => c.textContent)).toEqual(["fastgateway", "6", "50k", "10k", "60k", "0%"]);
    expect(within(rows[1]).getAllByRole("cell").slice(0, 6).map((c) => c.textContent)).toEqual(["acme", "4", "40k", "8k", "48k", "0%"]);
    expect(within(clients).getByRole("link", { name: "fastgateway" })).toHaveAttribute("href", "/api-clients/fastgateway");
    // A tenant is text the caller wrote: shown as text, never as markup.
    expect(clients.querySelector("img")).toBeNull();
  });

  it("says so when no API client used anything", async () => {
    fakeApi({ "GET /api/me": { body: operator }, [`GET ${overviewPath}`]: { body: { ...usageOverview, clients: [] } } });
    renderApp("/admin/usage");
    const clients = await screen.findByRole("region", { name: "API clients" });
    expect(within(clients).getByText("No usage in this period.")).toBeInTheDocument();
  });

  it("links a repository to its page, and a removed one only to its CSV", async () => {
    fakeApi({ "GET /api/me": { body: operator }, [`GET ${overviewPath}`]: { body: usageOverview } });
    renderApp("/admin/usage");
    const repos = await screen.findByRole("region", { name: "Repositories" });
    expect(within(repos).getByRole("link", { name: "main/acme/payments" })).toHaveAttribute("href", "/admin/usage/main/acme/payments");
    const off = within(repos).getByText("main/acme/old").closest("tr") as HTMLElement;
    expect(within(off).getByText("removed")).toBeInTheDocument();
    expect(repos).not.toHaveTextContent("switched off");
    expect(within(off).queryByRole("link", { name: "main/acme/old" })).not.toBeInTheDocument();
    expect(within(off).getByRole("link", { name: "CSV" })).toHaveAttribute("href", "/api/admin/usage.csv?days=30&instance=main&project=acme%2Fold");
    // An active repository has its page, so no CSV link on its row.
    const on = within(repos).getByRole("link", { name: "main/acme/payments" }).closest("tr") as HTMLElement;
    expect(within(on).queryByRole("link", { name: "CSV" })).not.toBeInTheDocument();
  });

  it("says paused, linked to its page, for a paused repository", async () => {
    const paused = { ...usageOverview.repositories[0], project: "acme/tools", switchedOn: false, paused: true, reviews: 1 };
    fakeApi({
      "GET /api/me": { body: operator },
      [`GET ${overviewPath}`]: { body: { ...usageOverview, repositories: [...usageOverview.repositories, paused] } },
    });
    renderApp("/admin/usage");
    const repos = await screen.findByRole("region", { name: "Repositories" });
    const link = await within(repos).findByRole("link", { name: "main/acme/tools" });
    expect(link).toHaveAttribute("href", "/admin/usage/main/acme/tools");
    const row = link.closest("tr") as HTMLElement;
    expect(within(row).getByText("paused")).toHaveClass("pill", "grey");
    expect(within(row).queryByText("removed")).not.toBeInTheDocument();
  });

  it("reads another period when chosen", async () => {
    const fake = fakeApi({
      "GET /api/me": { body: operator },
      [`GET ${overviewPath}`]: { body: usageOverview },
      "GET /api/admin/usage?days=7": { body: { ...usageOverview, days: 7, reviews: 5 } },
    });
    renderApp("/admin/usage");
    await screen.findByRole("region", { name: "Repositories" });
    await userEvent.selectOptions(screen.getByRole("combobox", { name: "Period" }), "Last 7 days");
    await vi.waitFor(() => expect(fake.find("GET", "/api/admin/usage?days=7")).toHaveLength(1));
    // The CSV follows the period.
    expect(await screen.findByRole("link", { name: "Download CSV" })).toHaveAttribute("href", "/api/admin/usage.csv?days=7");
  });

  it("downloads the CSV with a plain link", async () => {
    fakeApi({ "GET /api/me": { body: operator }, [`GET ${overviewPath}`]: { body: usageOverview } });
    renderApp("/admin/usage");
    const link = await screen.findByRole("link", { name: "Download CSV" });
    expect(link.tagName).toBe("A");
    expect(link).toHaveAttribute("href", "/api/admin/usage.csv?days=30");
  });

  it("draws a day's share as a progress element, against the busiest day", async () => {
    fakeApi({ "GET /api/me": { body: operator }, [`GET ${overviewPath}`]: { body: usageOverview } });
    renderApp("/admin/usage");
    const daily = await screen.findByRole("region", { name: "Daily" });
    const bars = within(daily).getAllByRole("progressbar");
    expect(bars.map((b) => [b.getAttribute("value"), b.getAttribute("max")])).toEqual([
      ["250000", "750000"],
      ["750000", "750000"],
      ["566000", "750000"],
    ]);
    expect(document.querySelector("[style]")).toBeNull();
  });

  it("says days end at midnight UTC and totals count all tokens", async () => {
    fakeApi({ "GET /api/me": { body: operator }, [`GET ${overviewPath}`]: { body: usageOverview } });
    renderApp("/admin/usage");
    expect(await screen.findByText(/midnight UTC/)).toHaveTextContent("Totals count all tokens: input, cache read, cache write, output and reasoning.");
  });

  it("shows Not found to a user and never asks for the usage", async () => {
    const fake = fakeApi({ "GET /api/me": { body: viewer }, [`GET ${overviewPath}`]: { body: usageOverview } });
    renderApp("/admin/usage");
    expect(await screen.findByRole("heading", { name: "Not found" })).toBeInTheDocument();
    expect(fake.calls.filter((c) => c.url.startsWith("/api/admin/usage"))).toHaveLength(0);
  });

  it("shows the error when the usage can't be read", async () => {
    fakeApi({ "GET /api/me": { body: operator }, [`GET ${overviewPath}`]: { status: 500, body: { error: "could not read the usage" } } });
    renderApp("/admin/usage");
    expect(await screen.findByRole("alert")).toHaveTextContent("could not read the usage");
  });

  it("is in the admin menu and on the admin home", async () => {
    fakeApi({ "GET /api/me": { body: operator }, [`GET ${overviewPath}`]: { body: usageOverview } });
    const { unmount } = renderApp("/admin/usage");
    const nav = await screen.findByRole("navigation", { name: "Main" });
    expect(within(nav).getByRole("link", { name: /Usage/ })).toHaveAttribute("href", "/admin/usage");
    unmount();
    fakeApi({ "GET /api/me": { body: operator } });
    renderApp("/admin");
    const home = await screen.findByRole("region", { name: "Admin pages" });
    expect(within(home).getByRole("link", { name: "Usage" })).toHaveAttribute("href", "/admin/usage");
    expect(within(home).getByText("Where model tokens go.")).toBeInTheDocument();
  });
});

describe("Usage of a repository", () => {
  const routes = () => ({ "GET /api/me": { body: operator }, [`GET ${repoPath}`]: { body: usageRepository } });

  it("has a section for skills, merge requests, models, days, waste and cost per review", async () => {
    fakeApi(routes());
    renderApp("/admin/usage/main/acme/payments");
    expect(await screen.findByRole("heading", { name: "acme/payments" })).toBeInTheDocument();
    for (const name of ["Skills", "Merge requests", "Models", "Daily", "Wasted", "Cost per review"]) {
      expect(await screen.findByRole("heading", { level: 2, name })).toBeInTheDocument();
    }
  });

  it("splits the tokens by global and team keys", async () => {
    fakeApi(routes());
    renderApp("/admin/usage/main/acme/payments");
    expect(await screen.findByText("Tokens on global keys: 800k · on team keys: 300k")).toBeInTheDocument();
  });

  it("shows a merge request title as text and links to the merge request", async () => {
    fakeApi(routes());
    renderApp("/admin/usage/main/acme/payments");
    const title = "<img src=x onerror=alert(1)>";
    expect(await screen.findByText(title)).toBeInTheDocument();
    expect(document.querySelector("img")).toBeNull();
    expect(screen.getByRole("link", { name: "Fix the ledger" })).toHaveAttribute("href", "https://gitlab.example.com/acme/payments/-/merge_requests/77");
  });

  it("links a merge request's number to its review page", async () => {
    fakeApi(routes());
    renderApp("/admin/usage/main/acme/payments");
    const mrs = await screen.findByRole("region", { name: "Merge requests" });
    expect(within(mrs).getByRole("link", { name: "77" })).toHaveAttribute("href", "/reviews/main/acme/payments/77");
    expect(within(mrs).getByRole("link", { name: "78" })).toHaveAttribute("href", "/reviews/main/acme/payments/78");
  });

  it("counts failed runs for skills only, since a model has none to count", async () => {
    fakeApi(routes());
    renderApp("/admin/usage/main/acme/payments");
    const skills = await screen.findByRole("region", { name: "Skills" });
    const models = screen.getByRole("region", { name: "Models" });
    expect(within(skills).getByRole("columnheader", { name: "Failed" })).toBeInTheDocument();
    expect(within(models).queryByRole("columnheader", { name: "Failed" })).toBeNull();
    // The model's row keeps its runs and tokens.
    expect(within(models).getByText("claude-sonnet").closest("tr")).toHaveTextContent("21");
  });

  it("shows the models of each merge request as provider / model, the name alone without a provider", async () => {
    fakeApi({
      "GET /api/me": { body: operator },
      [`GET ${repoPath}`]: {
        body: {
          ...usageRepository,
          mergeRequests: [
            {
              ...usageRepository.mergeRequests[0],
              models: [
                { provider: "anthropic", model: "claude-sonnet" },
                { provider: "openrouter", model: "claude-sonnet" },
              ],
            },
            { ...usageRepository.mergeRequests[1], models: [{ provider: "", model: "old-model" }] },
          ],
        },
      },
    });
    renderApp("/admin/usage/main/acme/payments");
    const mrs = await screen.findByRole("region", { name: "Merge requests" });
    const first = within(mrs).getByRole("link", { name: "77" }).closest("tr") as HTMLElement;
    expect(first).toHaveTextContent("anthropic / claude-sonnet");
    expect(first).toHaveTextContent("openrouter / claude-sonnet");
    const second = within(mrs).getByRole("link", { name: "78" }).closest("tr") as HTMLElement;
    expect(second).toHaveTextContent("old-model");
    expect(second).not.toHaveTextContent(" / old-model");
  });

  it("shows a merge request that ran on no recorded model with no models", async () => {
    fakeApi(routes());
    renderApp("/admin/usage/main/acme/payments");
    const mrs = await screen.findByRole("region", { name: "Merge requests" });
    expect(within(mrs).getByRole("link", { name: "78" }).closest("tr")).not.toHaveTextContent("claude");
  });

  it("shows a provider or model as text, never as HTML", async () => {
    fakeApi({
      "GET /api/me": { body: operator },
      [`GET ${repoPath}`]: {
        body: {
          ...usageRepository,
          mergeRequests: [{ ...usageRepository.mergeRequests[0], models: [{ provider: "<img src=x onerror=alert(1)>", model: "<b>m</b>" }] }],
          models: [{ ...usageRepository.models[0], provider: "<img src=y onerror=alert(2)>", name: "<i>n</i>" }],
        },
      },
    });
    renderApp("/admin/usage/main/acme/payments");
    expect(await screen.findByText(/<img src=x onerror=alert\(1\)> \/ <b>m<\/b>/)).toBeInTheDocument();
    expect(screen.getByText("<img src=y onerror=alert(2)>")).toBeInTheDocument();
    expect(document.querySelector("img, i")).toBeNull();
  });

  it("lists one model name under two providers as two rows, each with its provider", async () => {
    const error = vi.spyOn(console, "error").mockImplementation(() => undefined);
    fakeApi({
      "GET /api/me": { body: operator },
      [`GET ${repoPath}`]: {
        body: {
          ...usageRepository,
          models: [
            { provider: "anthropic", name: "claude-sonnet", tokens: { ...usageRepository.models[0].tokens }, runs: 21, failed: 0 },
            { provider: "openrouter", name: "claude-sonnet", tokens: { ...usageRepository.models[0].tokens, in: 5_000, out: 1_000 }, runs: 4, failed: 0 },
          ],
        },
      },
    });
    renderApp("/admin/usage/main/acme/payments");
    const models = await screen.findByRole("region", { name: "Models" });
    expect(within(models).getByRole("columnheader", { name: "Provider" })).toBeInTheDocument();
    const rows = within(models).getAllByRole("row").slice(1);
    expect(rows).toHaveLength(2);
    expect(rows[0]).toHaveTextContent("anthropic");
    expect(rows[0]).toHaveTextContent("claude-sonnet");
    expect(rows[0]).toHaveTextContent("21");
    expect(rows[1]).toHaveTextContent("openrouter");
    expect(rows[1]).toHaveTextContent("claude-sonnet");
    expect(rows[1]).toHaveTextContent("4");
    // Two rows with one key would make React warn.
    expect(error).not.toHaveBeenCalled();
    error.mockRestore();
  });

  it("says unknown cache counts are unknown for the skill runs, and shows no percent for them", async () => {
    fakeApi(routes());
    renderApp("/admin/usage/main/acme/payments");
    const security = (await screen.findByText("security")).closest("tr") as HTMLElement;
    expect(within(security).getByLabelText(/unknown for 3 skill runs/)).toBeInTheDocument();
    expect(within(security).getAllByRole("cell").at(-1)).toHaveTextContent("—");
    // Reported counts show as they are: 7k of 408k is 2%.
    const style = screen.getByText("style").closest("tr") as HTMLElement;
    expect(within(style).getByLabelText(/cache read 7k · cache write 1k · output 100k · reasoning 2k/)).toBeInTheDocument();
    expect(within(style).queryByLabelText(/unknown/)).toBeNull();
    expect(within(style).getAllByRole("cell").at(-1)).toHaveTextContent("2%");
  });

  it("shows failed and re-run tokens as separate lines, never added", async () => {
    fakeApi(routes());
    renderApp("/admin/usage/main/acme/payments");
    const wasted = await screen.findByRole("region", { name: "Wasted" });
    const failed = within(wasted).getByText("Failed skills").closest("tr") as HTMLElement;
    const reruns = within(wasted).getByText(/Re-runs someone asked for/).closest("tr") as HTMLElement;
    expect(failed).toHaveTextContent("45k");
    expect(reruns).toHaveTextContent("100k");
    expect(reruns).toHaveTextContent("2");
    expect(wasted).not.toHaveTextContent("145k");
    expect(wasted).toHaveTextContent("in both");
    expect(wasted).toHaveTextContent("1 review without a result");
    expect(wasted).toHaveTextContent("3 superseded");
  });

  it("shows a dash for a median with no reviews of that kind", async () => {
    fakeApi(routes());
    renderApp("/admin/usage/main/acme/payments");
    const cost = await screen.findByRole("region", { name: "Cost per review" });
    expect(within(cost).getByText("Full review median").nextElementSibling).toHaveTextContent("120k");
    expect(within(cost).getByText("Incremental review median").nextElementSibling).toHaveTextContent("—");
  });

  it("links an outlier to its review page", async () => {
    fakeApi(routes());
    renderApp("/admin/usage/main/acme/payments");
    const cost = await screen.findByRole("region", { name: "Cost per review" });
    expect(within(cost).getByRole("link", { name: /77/ })).toHaveAttribute("href", "/reviews/main/acme/payments/77");
    expect(cost).toHaveTextContent("450k");
  });

  it("reads another period, and offers this repository's CSV", async () => {
    const fake = fakeApi({
      ...routes(),
      "GET /api/repositories/main/acme/payments/usage?days=7": { body: { ...usageRepository, days: 7 } },
    });
    renderApp("/admin/usage/main/acme/payments");
    expect(await screen.findByRole("link", { name: "Download CSV" })).toHaveAttribute("href", "/api/admin/usage.csv?days=30&instance=main&project=acme%2Fpayments");
    await userEvent.selectOptions(screen.getByRole("combobox", { name: "Period" }), "Last 7 days");
    await vi.waitFor(() => expect(fake.find("GET", "/api/repositories/main/acme/payments/usage?days=7")).toHaveLength(1));
  });

  it("shows Not found for a repository without usage pages", async () => {
    fakeApi({ "GET /api/me": { body: operator } });
    renderApp("/admin/usage/main/acme/old");
    expect(await screen.findByRole("heading", { name: "Not found" })).toBeInTheDocument();
  });

  it("shows Not found to a user and never asks for the usage", async () => {
    const fake = fakeApi({ "GET /api/me": { body: viewer }, [`GET ${repoPath}`]: { body: usageRepository } });
    renderApp("/admin/usage/main/acme/payments");
    expect(await screen.findByRole("heading", { name: "Not found" })).toBeInTheDocument();
    expect(fake.calls.filter((c) => c.url.includes("/usage"))).toHaveLength(0);
  });

  it("shows the error when the usage can't be read", async () => {
    fakeApi({ "GET /api/me": { body: operator }, [`GET ${repoPath}`]: { status: 500, body: { error: "could not read the usage" } } });
    renderApp("/admin/usage/main/acme/payments");
    expect(await screen.findByRole("alert")).toHaveTextContent("could not read the usage");
  });
});
