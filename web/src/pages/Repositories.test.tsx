import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { fakeApi } from "../test/fakeApi";
import { platformAdmin, viewer, repoDetail, repoPayments, repoWidgets } from "../test/fixtures";
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

  it("lets an admin switch a repository on, with the CSRF token", async () => {
    const fake = fakeApi({
      "GET /api/me": { body: platformAdmin },
      "GET /api/repositories": { body: [repoWidgets] },
      "GET /api/repositories/available?search=": {
        body: { items: [{ instance: "main", project: "acme/payments", platform: "gitlab" }], truncated: true },
      },
      "GET /api/repositories/available?search=pay": {
        body: { items: [{ instance: "main", project: "acme/payments", platform: "gitlab" }], truncated: false },
      },
      "POST /api/repositories": { status: 201, body: repoDetail(repoPayments) },
    });
    renderApp("/repos");
    await userEvent.click(await screen.findByRole("button", { name: "+ Add repository" }));
    const picker = await screen.findByRole("region", { name: "Add repository" });
    expect(await within(picker).findByText(/Showing the first 500/)).toBeInTheDocument();
    await userEvent.type(within(picker).getByRole("searchbox", { name: "Search" }), "pay");
    await userEvent.click(within(picker).getByRole("button", { name: "Search" }));
    expect(await within(picker).findByText("acme/payments")).toBeInTheDocument();
    expect(fake.find("GET", "/api/repositories/available?search=pay")).toHaveLength(1);

    await userEvent.click(within(picker).getByRole("button", { name: "Switch on acme/payments" }));
    const [post] = fake.find("POST", "/api/repositories");
    expect(post.headers.get("X-CSRF-Token")).toBe("csrf-123");
    expect(post.body).toEqual({ project: "acme/payments" });
    expect(await screen.findByTestId("location")).toHaveTextContent("/repos/main/acme/payments");
  });

  it("opens the picker from the overview's Add repository", async () => {
    fakeApi({
      "GET /api/me": { body: platformAdmin },
      "GET /api/repositories": { body: [] },
      "GET /api/repositories/available?search=": { status: 502, body: { error: "could not ask GitLab" } },
    });
    renderApp("/repos?add=1");
    const picker = await screen.findByRole("region", { name: "Add repository" });
    expect(await within(picker).findByText("could not ask GitLab")).toBeInTheDocument();
  });

  it("lets an admin switch off a dashboard repository, not a config one", async () => {
    vi.stubGlobal("confirm", vi.fn(() => true));
    const fake = fakeApi({
      "GET /api/me": { body: platformAdmin },
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
