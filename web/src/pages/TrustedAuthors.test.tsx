import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import type { RepositoryDetailView } from "../api";
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

const withTrust = (d: RepositoryDetailView, patch: Partial<RepositoryDetailView["settings"]>): RepositoryDetailView => ({
  ...d,
  settings: { ...d.settings, ...patch },
});

const settingsRoutes = (detail: RepositoryDetailView) => ({
  "GET /api/me": { body: ssoAdmin },
  "GET /api/repositories/main/acme/payments": { body: detail },
  "GET /api/skills": { body: librarySkills },
  "GET /api/repositories/main/acme/payments/inputs": { body: noInputs },
  "GET /api/repositories/main/acme/payments/teams": { body: { teams: [] } },
  "PUT /api/repositories/main/acme/payments/settings": { body: detail },
});

describe("Trusted authors: repository settings", () => {
  it("offers the instance default, on and off, and sends off", async () => {
    const fake = fakeApi(settingsRoutes(repoDetail(repoPayments)));
    renderApp("/repos/main/acme/payments?tab=settings");
    const select = await screen.findByLabelText("Trusted authors only");
    expect(within(select).getByRole("option", { name: "Instance default (follow visibility)" })).toBeInTheDocument();
    await userEvent.selectOptions(select, "off");
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    const [put] = fake.find("PUT", "/api/repositories/main/acme/payments/settings");
    expect(put.body).toEqual({ trustedAuthorsOnly: "off" });
  });

  it("sends null to follow the instance again", async () => {
    const detail = withTrust(repoDetail(repoPayments), { trustedAuthorsOnly: { value: "on", overridden: true } });
    const fake = fakeApi(settingsRoutes(detail));
    renderApp("/repos/main/acme/payments?tab=settings");
    const select = await screen.findByLabelText("Trusted authors only");
    expect(select).toHaveValue("on");
    await userEvent.selectOptions(select, "");
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(fake.find("PUT", "/api/repositories/main/acme/payments/settings")[0].body).toEqual({ trustedAuthorsOnly: null });
  });

  it("adds a username to the allowlist and removes one, keeping the others' IDs", async () => {
    const detail = withTrust(repoDetail(repoPayments), {
      trustedAuthors: { value: [{ id: 9, username: "bob" }, { id: 4, username: "carol" }], overridden: true },
    });
    const fake = fakeApi(settingsRoutes(detail));
    renderApp("/repos/main/acme/payments?tab=settings");
    const list = await screen.findByRole("group", { name: "Trusted authors" });
    expect(list).toHaveTextContent("bob");
    await userEvent.click(within(list).getByRole("button", { name: "Remove carol" }));
    await userEvent.type(within(list).getByLabelText("Add a username"), "alice");
    await userEvent.click(within(list).getByRole("button", { name: "Add" }));
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(fake.find("PUT", "/api/repositories/main/acme/payments/settings")[0].body).toEqual({
      trustedAuthors: [{ id: 9, username: "bob" }, { username: "alice" }],
    });
  });

  it("sends null when the last name goes", async () => {
    const detail = withTrust(repoDetail(repoPayments), { trustedAuthors: { value: [{ id: 9, username: "bob" }], overridden: true } });
    const fake = fakeApi(settingsRoutes(detail));
    renderApp("/repos/main/acme/payments?tab=settings");
    const list = await screen.findByRole("group", { name: "Trusted authors" });
    await userEvent.click(within(list).getByRole("button", { name: "Remove bob" }));
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(fake.find("PUT", "/api/repositories/main/acme/payments/settings")[0].body).toEqual({ trustedAuthors: null });
  });

  it("shows both read-only for a repository the config sets up", async () => {
    const detail = withTrust(repoDetail(repoWidgets), { trustedAuthors: { value: [{ id: 9, username: "bob" }], overridden: true } });
    fakeApi({
      "GET /api/me": { body: ssoAdmin },
      "GET /api/repositories/main/acme/widgets": { body: detail },
      "GET /api/skills": { body: librarySkills },
      "GET /api/repositories/main/acme/widgets/inputs": { body: noInputs },
      "GET /api/repositories/main/acme/widgets/teams": { body: { teams: [] } },
    });
    renderApp("/repos/main/acme/widgets?tab=settings");
    expect(await screen.findByLabelText("Trusted authors only")).toBeDisabled();
    const list = screen.getByRole("group", { name: "Trusted authors" });
    expect(list).toHaveTextContent("bob");
    expect(within(list).queryByRole("button", { name: "Remove bob" })).not.toBeInTheDocument();
    expect(within(list).queryByLabelText("Add a username")).not.toBeInTheDocument();
  });
});

describe("Trusted authors: maintainers", () => {
  it("shows a maintainer who is trusted, read-only, and saves without it", async () => {
    const detail = withTrust(repoDetail(repoPayments, "maintainer"), {
      trustedAuthors: { value: [{ id: 9, username: "bob" }], overridden: true },
    });
    const fake = fakeApi(settingsRoutes(detail));
    renderApp("/repos/main/acme/payments?tab=settings");
    expect(await screen.findByLabelText("Trusted authors only")).toBeDisabled();
    const list = screen.getByRole("group", { name: "Trusted authors" });
    expect(list).toHaveTextContent("bob");
    expect(within(list).queryByRole("button", { name: "Remove bob" })).not.toBeInTheDocument();
    expect(list).toHaveTextContent("Only admins of this repository change who is trusted.");
    await userEvent.click(screen.getByRole("switch", { name: "Review drafts" }));
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    const body = fake.find("PUT", "/api/repositories/main/acme/payments/settings")[0].body as Record<string, unknown>;
    expect(Object.keys(body)).toEqual(["reviewDrafts"]);
  });
});

describe("Trusted authors: instances", () => {
  it("sets a dashboard instance's default", async () => {
    const fake = fakeApi({
      "GET /api/me": { body: operator },
      "GET /api/admin/instances": { body: [instanceGitLab, instanceDashboardGitLab] },
      "PUT /api/admin/instances/extra/trusted-authors": { status: 204 },
    });
    renderApp("/admin/integrations");
    const extra = await screen.findByRole("region", { name: "GitLab · extra" });
    await userEvent.selectOptions(within(extra).getByLabelText("Trusted authors only"), "off");
    await vi.waitFor(() => expect(fake.find("PUT", "/api/admin/instances/extra/trusted-authors")).toHaveLength(1));
    expect(fake.find("PUT", "/api/admin/instances/extra/trusted-authors")[0].body).toEqual({ mode: "off" });
  });

  it("shows a config instance's default read-only", async () => {
    fakeApi({ "GET /api/me": { body: operator }, "GET /api/admin/instances": { body: [{ ...instanceGitLab, trustedAuthorsOnly: "off" }] } });
    renderApp("/admin/integrations");
    const main = await screen.findByRole("region", { name: "GitLab · main" });
    const select = within(main).getByLabelText("Trusted authors only");
    expect(select).toBeDisabled();
    expect(select).toHaveValue("off");
  });
});

describe("Trusted authors: waiting changes", () => {
  const waiting = (role: "admin" | "viewer") => ({
    ...repoDetail(repoPayments, role),
    waiting: [
      { number: 12, title: "Fix it", author: "stranger", webUrl: "https://gitlab.example.com/acme/payments/-/merge_requests/12", since: "2026-11-08T08:00:00Z", reason: "untrusted_author" as const },
      { number: 13, title: "Other", author: "someone", webUrl: "", since: "2026-11-08T07:00:00Z", reason: "trust_check_failed" as const },
    ],
  });

  it("lists them for a maintainer with Start review, which starts one", async () => {
    const fake = fakeApi({
      "GET /api/me": { body: ssoAdmin },
      "GET /api/repositories/main/acme/payments": { body: waiting("admin") },
      "GET /api/reviews?project=acme%2Fpayments&days=90": { body: { items: [], truncated: false } },
      "POST /api/reviews/main/acme/payments/12/start": { status: 202, body: { status: "queued" } },
    });
    renderApp("/repos/main/acme/payments");
    const box = await screen.findByRole("region", { name: "Waiting for a start" });
    expect(box).toHaveTextContent("!12 by stranger");
    expect(box).toHaveTextContent("Couldn't check the author's access");
    await userEvent.click(within(box).getByRole("button", { name: "Start review of !12" }));
    const [post] = fake.find("POST", "/api/reviews/main/acme/payments/12/start");
    expect(post.headers.get("X-CSRF-Token")).toBe(ssoAdmin.csrfToken);
  });

  it("writes a GitHub pull request with #", async () => {
    fakeApi({
      "GET /api/me": { body: viewer },
      "GET /api/repositories/main/acme/payments": { body: { ...waiting("viewer"), repo: { ...repoPayments, platform: "github" } } },
      "GET /api/reviews?project=acme%2Fpayments&days=90": { body: { items: [], truncated: false } },
    });
    renderApp("/repos/main/acme/payments");
    const box = await screen.findByRole("region", { name: "Waiting for a start" });
    expect(box).toHaveTextContent("#12 by stranger");
  });

  it("shows a viewer the list without the button", async () => {
    fakeApi({
      "GET /api/me": { body: viewer },
      "GET /api/repositories/main/acme/payments": { body: waiting("viewer") },
      "GET /api/reviews?project=acme%2Fpayments&days=90": { body: { items: [], truncated: false } },
    });
    renderApp("/repos/main/acme/payments");
    const box = await screen.findByRole("region", { name: "Waiting for a start" });
    expect(box).toHaveTextContent("!12 by stranger");
    expect(within(box).queryByRole("button")).not.toBeInTheDocument();
  });
});
