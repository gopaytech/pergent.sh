import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { fakeApi } from "../test/fakeApi";
import {
  instanceDashboardGitLab,
  instanceGitLab,
  librarySkills,
  operator,
  ssoAdmin,
  repoDetail,
  repoPayments,
  repoWidgets,
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
        body: { items: [{ instance: "main", project: "acme/payments", platform: "gitlab" }], page: 1, hasMore: false, truncated: false },
      },
      "GET /api/admin/instances/main/repositories?search=pay": {
        body: { items: [{ instance: "main", project: "acme/payments", platform: "gitlab" }], page: 1, hasMore: false, truncated: false },
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
    expect(await screen.findByTestId("location")).toHaveTextContent("/repos/main/acme/payments");
  });

  const gitlabPage = (from: number, count: number, page: number, hasMore: boolean, truncated = false) => ({
    items: Array.from({ length: count }, (_, i) => ({
      instance: "main",
      project: `acme/repo-${String(from + i).padStart(3, "0")}`,
      platform: "gitlab",
    })),
    page,
    hasMore,
    truncated,
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
        body: { items: [{ instance: "main", project: "acme/payments", platform: "gitlab" }], page: 1, hasMore: true, truncated: false },
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
      "GET /api/admin/instances/main/repositories?search=": { body: { items: [], page: 1, hasMore: true, truncated: true } },
    });
    const picker = await openPicker();
    expect(await within(picker).findByText(/too long to read whole/)).toBeInTheDocument();
    // A page can be empty and not the last: no "nothing" yet, but Load more.
    expect(within(picker).queryByText("No repository to switch on.")).not.toBeInTheDocument();
    expect(within(picker).getByRole("button", { name: "Load more" })).toBeInTheDocument();
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
      "GET /api/admin/instances/main/repositories?search=": { body: { items: [], page: 1, hasMore: false, truncated: false } },
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

  it("lets an admin switch off a dashboard repository, not a config one", async () => {
    vi.stubGlobal("confirm", vi.fn(() => true));
    const fake = fakeApi({
      "GET /api/me": { body: ssoAdmin },
      "GET /api/repositories": { body: [repoPayments, repoWidgets] },
      "DELETE /api/repositories/main/acme/payments": { body: { ...repoDetail(repoPayments), repo: { ...repoPayments, on: false } } },
    });
    renderApp("/repos");
    const widgets = await screen.findByRole("switch", { name: "Review acme/widgets" });
    expect(widgets).toBeDisabled();
    const payments = screen.getByRole("switch", { name: "Review acme/payments" });
    expect(payments).toBeChecked();
    await userEvent.click(payments);
    const [del] = fake.find("DELETE", "/api/repositories/main/acme/payments");
    expect(del.headers.get("X-CSRF-Token")).toBe("csrf-123");
  });
});
