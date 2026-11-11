import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { fakeApi } from "../test/fakeApi";
import { operator, usageOverview, usageRepository, viewer } from "../test/fixtures";
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
    expect(rows.map((r) => within(r).getAllByRole("cell")[0].textContent)).toEqual(["main/acme/payments", "main/acme/widgets", "main/acme/oldswitched offCSV"]);
    // Input plus output, not the cache: 1,255k + 311k.
    expect(within(screen.getByRole("region", { name: "Totals" })).getByText("1.6M")).toBeInTheDocument();
    expect(within(rows[0]).getAllByRole("cell").slice(0, 5).map((c) => c.textContent)).toEqual(["main/acme/payments", "30", "900k", "200k", "1.1M"]);
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
    expect(within(rows[0]).getAllByRole("cell").slice(0, 5).map((c) => c.textContent)).toEqual(["fastgateway", "6", "50k", "10k", "60k"]);
    expect(within(rows[1]).getAllByRole("cell").slice(0, 5).map((c) => c.textContent)).toEqual(["acme", "4", "40k", "8k", "48k"]);
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

  it("links a repository to its page, and a switched-off one only to its CSV", async () => {
    fakeApi({ "GET /api/me": { body: operator }, [`GET ${overviewPath}`]: { body: usageOverview } });
    renderApp("/admin/usage");
    const repos = await screen.findByRole("region", { name: "Repositories" });
    expect(within(repos).getByRole("link", { name: "main/acme/payments" })).toHaveAttribute("href", "/admin/usage/main/acme/payments");
    const off = within(repos).getByText("main/acme/old").closest("tr") as HTMLElement;
    expect(within(off).getByText("switched off")).toBeInTheDocument();
    expect(within(off).queryByRole("link", { name: "main/acme/old" })).not.toBeInTheDocument();
    expect(within(off).getByRole("link", { name: "CSV" })).toHaveAttribute("href", "/api/admin/usage.csv?days=30&instance=main&project=acme%2Fold");
    // An active repository has its page, so no CSV link on its row.
    const on = within(repos).getByRole("link", { name: "main/acme/payments" }).closest("tr") as HTMLElement;
    expect(within(on).queryByRole("link", { name: "CSV" })).not.toBeInTheDocument();
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

  it("says days end at midnight UTC and totals add input and output only", async () => {
    fakeApi({ "GET /api/me": { body: operator }, [`GET ${overviewPath}`]: { body: usageOverview } });
    renderApp("/admin/usage");
    expect(await screen.findByText(/midnight UTC/)).toHaveTextContent(/input and output tokens only/);
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

  it("says unknown cache counts are unknown for the skill runs, never zero", async () => {
    fakeApi(routes());
    renderApp("/admin/usage/main/acme/payments");
    const security = (await screen.findByText("security")).closest("tr") as HTMLElement;
    expect(security).toHaveTextContent("unknown for 3 skill runs");
    expect(security).not.toHaveTextContent("cache read 0");
    // Reported counts show as they are.
    const style = screen.getByText("style").closest("tr") as HTMLElement;
    expect(style).toHaveTextContent("cache read 7k");
    expect(style).toHaveTextContent("reasoning 2k");
    expect(style).not.toHaveTextContent("unknown");
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
