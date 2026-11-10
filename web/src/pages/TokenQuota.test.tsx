import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import type { QuotaView, RepositoryDetailView, WaitingView } from "../api";
import { fakeApi } from "../test/fakeApi";
import {
  instanceDashboardGitLab,
  instanceGitLab,
  librarySkills,
  operator,
  repoDetail,
  repoPayments,
  repoWidgets,
  ssoAdmin,
  viewer,
} from "../test/fixtures";
import { renderApp } from "../test/render";

afterEach(() => vi.unstubAllGlobals());

const noInputs = { branch: "main", skills: [], detected: [], instructions: "" };
const noReviews = { items: [], truncated: false };

const withQuota = (d: RepositoryDetailView, value: number | null): RepositoryDetailView => ({
  ...d,
  settings: { ...d.settings, monthlyTokenQuota: { value, overridden: value !== null } },
});

const settingsRoutes = (detail: RepositoryDetailView) => ({
  "GET /api/me": { body: ssoAdmin },
  "GET /api/repositories/main/acme/payments": { body: detail },
  "GET /api/skills": { body: librarySkills },
  "GET /api/repositories/main/acme/payments/inputs": { body: noInputs },
  "GET /api/repositories/main/acme/payments/teams": { body: { teams: [] } },
  "PUT /api/repositories/main/acme/payments/settings": { body: detail },
});

const pageRoutes = (detail: RepositoryDetailView) => ({
  "GET /api/me": { body: ssoAdmin },
  "GET /api/repositories/main/acme/payments": { body: detail },
  "GET /api/reviews?project=acme%2Fpayments&days=90": { body: noReviews },
});

describe("Token quota: repository settings", () => {
  it("sends the repository quota, stripped of separators", async () => {
    const fake = fakeApi(settingsRoutes(repoDetail(repoPayments)));
    renderApp("/repos/main/acme/payments?tab=settings");
    const input = await screen.findByLabelText("Monthly token quota");
    expect(screen.getByText("Tokens per calendar month (UTC), input plus output. Empty: limited only by the instance's quota.")).toBeInTheDocument();
    await userEvent.type(input, "5,000,000");
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    const [put] = fake.find("PUT", "/api/repositories/main/acme/payments/settings");
    expect(put.body).toEqual({ monthlyTokenQuota: 5000000 });
  });

  it("sends null when emptied", async () => {
    const fake = fakeApi(settingsRoutes(withQuota(repoDetail(repoPayments), 10)));
    renderApp("/repos/main/acme/payments?tab=settings");
    const input = await screen.findByLabelText("Monthly token quota");
    expect(input).toHaveValue("10");
    await userEvent.clear(input);
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    const [put] = fake.find("PUT", "/api/repositories/main/acme/payments/settings");
    expect(put.body).toEqual({ monthlyTokenQuota: null });
  });

  it("refuses a non-number", async () => {
    const fake = fakeApi(settingsRoutes(repoDetail(repoPayments)));
    renderApp("/repos/main/acme/payments?tab=settings");
    await userEvent.type(await screen.findByLabelText("Monthly token quota"), "5M");
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(await screen.findByText(/whole number/)).toBeInTheDocument();
    expect(fake.find("PUT", "/api/repositories/main/acme/payments/settings")).toHaveLength(0);
  });

  it("hides the quota from a maintainer", async () => {
    fakeApi({
      ...settingsRoutes(repoDetail(repoPayments, "maintainer")),
      "GET /api/repositories/main/acme/payments": { body: repoDetail(repoPayments, "maintainer") },
    });
    renderApp("/repos/main/acme/payments?tab=settings");
    await screen.findByLabelText("Trusted authors only");
    expect(screen.queryByLabelText("Monthly token quota")).not.toBeInTheDocument();
    expect(screen.queryByRole("meter")).not.toBeInTheDocument();
  });

  it("shows a config repository's quota read-only", async () => {
    fakeApi({
      "GET /api/me": { body: ssoAdmin },
      "GET /api/repositories/main/acme/widgets": { body: withQuota(repoDetail(repoWidgets), 7) },
      "GET /api/skills": { body: librarySkills },
      "GET /api/repositories/main/acme/widgets/inputs": { body: noInputs },
      "GET /api/repositories/main/acme/widgets/teams": { body: { teams: [] } },
    });
    renderApp("/repos/main/acme/widgets?tab=settings");
    expect(await screen.findByLabelText("Monthly token quota")).toBeDisabled();
  });
});

describe("Token quota: usage on the repository page", () => {
  const quota = (repository: QuotaView["repository"], instance: QuotaView["instance"] = { used: 5, limit: null }): RepositoryDetailView => ({
    ...repoDetail(repoPayments),
    quota: { repository, instance },
  });

  it("shows amber from 80% and red when used up", async () => {
    fakeApi(pageRoutes(quota({ used: 80, limit: 100 })));
    const first = renderApp("/repos/main/acme/payments");
    const meter = await screen.findByRole("meter", { name: "Repository token quota" });
    expect(meter).toHaveClass("warn");
    expect(screen.getByText("Tokens this month: 80 of 100")).toBeInTheDocument();
    first.unmount();

    fakeApi(pageRoutes(quota({ used: 100, limit: 100 })));
    renderApp("/repos/main/acme/payments");
    const full = await screen.findByRole("meter", { name: "Repository token quota" });
    expect(full).toHaveClass("full");
    expect(full).not.toHaveClass("warn");
  });

  it("shows a calm bar below 80%", async () => {
    fakeApi(pageRoutes(quota({ used: 79, limit: 100 })));
    renderApp("/repos/main/acme/payments");
    const meter = await screen.findByRole("meter", { name: "Repository token quota" });
    expect(meter).not.toHaveClass("warn");
    expect(meter).not.toHaveClass("full");
  });

  it("shows usage without a bar when there is no limit", async () => {
    fakeApi(pageRoutes(quota({ used: 5, limit: 100 }, { used: 1234, limit: null })));
    renderApp("/repos/main/acme/payments");
    expect(await screen.findByText("Tokens this month: 1,234")).toBeInTheDocument();
    expect(screen.getByRole("meter", { name: "Repository token quota" })).toBeInTheDocument();
    expect(screen.queryByRole("meter", { name: "Instance token quota" })).not.toBeInTheDocument();
  });

  it("shows the instance's bar when it has a limit", async () => {
    fakeApi(pageRoutes(quota({ used: 5, limit: null }, { used: 2500, limit: 10000 })));
    renderApp("/repos/main/acme/payments");
    expect(await screen.findByText("Tokens this month: 2,500 of 10,000")).toBeInTheDocument();
    expect(screen.getByRole("meter", { name: "Instance token quota" })).not.toHaveClass("warn");
    expect(screen.queryByRole("meter", { name: "Repository token quota" })).not.toBeInTheDocument();
  });

  it("shows nothing to a maintainer", async () => {
    fakeApi({
      "GET /api/me": { body: ssoAdmin },
      "GET /api/repositories/main/acme/payments": { body: repoDetail(repoPayments, "maintainer") },
      "GET /api/reviews?project=acme%2Fpayments&days=90": { body: noReviews },
    });
    renderApp("/repos/main/acme/payments");
    await screen.findByRole("heading", { name: "acme/payments" });
    expect(screen.queryByText(/Tokens this month/)).not.toBeInTheDocument();
  });
});

describe("Token quota: waiting changes", () => {
  const held: WaitingView = {
    number: 12,
    head: "a".repeat(40),
    title: "Fix it",
    author: "alice",
    webUrl: "",
    since: "2026-11-08T08:00:00Z",
    reason: "quota_used_up",
  };
  const untrusted: WaitingView = { ...held, number: 14, head: "c".repeat(40), author: "stranger", reason: "untrusted_author" };

  it("offers Start on a quota-held change to admins only", async () => {
    const fake = fakeApi({
      ...pageRoutes({ ...repoDetail(repoPayments), waiting: [held] }),
      "POST /api/reviews/main/acme/payments/12/start": { status: 202, body: { status: "queued" } },
    });
    const first = renderApp("/repos/main/acme/payments");
    const box = await screen.findByRole("region", { name: "Waiting for a start" });
    expect(box).toHaveTextContent("Nobody reviews these changes until someone starts the review.");
    expect(box).toHaveTextContent("Monthly token quota used up.");
    await userEvent.click(within(box).getByRole("button", { name: "Start review of !12" }));
    expect(fake.find("POST", "/api/reviews/main/acme/payments/12/start")[0].body).toEqual({ head: "a".repeat(40) });
    first.unmount();

    fakeApi({
      "GET /api/me": { body: viewer },
      "GET /api/repositories/main/acme/payments": { body: { ...repoDetail(repoPayments, "maintainer"), waiting: [held] } },
      "GET /api/reviews?project=acme%2Fpayments&days=90": { body: noReviews },
    });
    renderApp("/repos/main/acme/payments");
    const seen = await screen.findByRole("region", { name: "Waiting for a start" });
    expect(seen).toHaveTextContent("Monthly token quota used up.");
    expect(within(seen).queryByRole("button")).not.toBeInTheDocument();
  });

  it("keeps Start on an untrusted author's change for a maintainer", async () => {
    fakeApi({
      "GET /api/me": { body: viewer },
      "GET /api/repositories/main/acme/payments": { body: { ...repoDetail(repoPayments, "maintainer"), waiting: [held, untrusted] } },
      "GET /api/reviews?project=acme%2Fpayments&days=90": { body: noReviews },
    });
    renderApp("/repos/main/acme/payments");
    const box = await screen.findByRole("region", { name: "Waiting for a start" });
    expect(within(box).getByRole("button", { name: "Start review of !14" })).toBeInTheDocument();
    expect(within(box).queryByRole("button", { name: "Start review of !12" })).not.toBeInTheDocument();
  });

  it("shows why a Start was refused", async () => {
    fakeApi({
      ...pageRoutes({ ...repoDetail(repoPayments), waiting: [held] }),
      "POST /api/reviews/main/acme/payments/12/start": { status: 409, body: { error: "the monthly token quota is used up" } },
    });
    renderApp("/repos/main/acme/payments");
    const box = await screen.findByRole("region", { name: "Waiting for a start" });
    await userEvent.click(within(box).getByRole("button", { name: "Start review of !12" }));
    expect(await within(box).findByRole("alert")).toHaveTextContent("the monthly token quota is used up");
  });
});

describe("Token quota: instances", () => {
  it("sets an instance's quota", async () => {
    const fake = fakeApi({
      "GET /api/me": { body: operator },
      "GET /api/admin/instances": { body: [instanceGitLab, { ...instanceDashboardGitLab, monthlyTokenQuota: 5000, tokensThisMonth: 4200 }] },
      "PUT /api/admin/instances/extra/token-quota": { status: 204 },
    });
    renderApp("/admin/integrations");
    const extra = await screen.findByRole("region", { name: "GitLab · extra" });
    expect(within(extra).getByText("Tokens this month: 4,200 of 5,000")).toBeInTheDocument();
    expect(within(extra).getByRole("meter", { name: "Instance token quota" })).toHaveClass("warn");
    const input = within(extra).getByLabelText("Monthly token quota");
    expect(input).toHaveValue("5000");
    await userEvent.clear(input);
    await userEvent.type(input, "1000");
    await userEvent.click(within(extra).getByRole("button", { name: "Save" }));
    await vi.waitFor(() => expect(fake.find("PUT", "/api/admin/instances/extra/token-quota")).toHaveLength(1));
    expect(fake.find("PUT", "/api/admin/instances/extra/token-quota")[0].body).toEqual({ monthlyTokenQuota: 1000 });
  });

  it("sends null when an instance's quota is emptied, and refuses a non-number", async () => {
    const fake = fakeApi({
      "GET /api/me": { body: operator },
      "GET /api/admin/instances": { body: [{ ...instanceDashboardGitLab, monthlyTokenQuota: 5000 }] },
      "PUT /api/admin/instances/extra/token-quota": { status: 204 },
    });
    renderApp("/admin/integrations");
    const extra = await screen.findByRole("region", { name: "GitLab · extra" });
    const input = within(extra).getByLabelText("Monthly token quota");
    await userEvent.clear(input);
    await userEvent.type(input, "1.5");
    await userEvent.click(within(extra).getByRole("button", { name: "Save" }));
    expect(await within(extra).findByText(/whole number/)).toBeInTheDocument();
    expect(fake.find("PUT", "/api/admin/instances/extra/token-quota")).toHaveLength(0);
    await userEvent.clear(input);
    await userEvent.click(within(extra).getByRole("button", { name: "Save" }));
    await vi.waitFor(() => expect(fake.find("PUT", "/api/admin/instances/extra/token-quota")).toHaveLength(1));
    expect(fake.find("PUT", "/api/admin/instances/extra/token-quota")[0].body).toEqual({ monthlyTokenQuota: null });
  });

  it("shows a config instance's quota read-only", async () => {
    fakeApi({
      "GET /api/me": { body: operator },
      "GET /api/admin/instances": { body: [{ ...instanceGitLab, monthlyTokenQuota: 9000, tokensThisMonth: 100 }] },
    });
    renderApp("/admin/integrations");
    const main = await screen.findByRole("region", { name: "GitLab · main" });
    const input = within(main).getByLabelText("Monthly token quota");
    expect(input).toBeDisabled();
    expect(input).toHaveValue("9000");
    expect(within(main).queryByRole("button", { name: "Save" })).not.toBeInTheDocument();
    expect(within(main).getByRole("meter", { name: "Instance token quota" })).toBeInTheDocument();
  });
});
