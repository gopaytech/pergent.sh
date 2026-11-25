import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { fakeApi } from "../test/fakeApi";
import { ssoAdmin, viewer, overview } from "../test/fixtures";
import { renderApp } from "../test/render";

afterEach(() => vi.unstubAllGlobals());

describe("Overview", () => {
  it("shows the numbers and the recent reviews, and no Needs attention panel", async () => {
    fakeApi({ "GET /api/me": { body: ssoAdmin }, "GET /api/overview?days=7": { body: overview } });
    renderApp("/");
    expect(await screen.findByRole("heading", { name: "Overview" })).toBeInTheDocument();

    const stats = await screen.findByRole("region", { name: "Numbers" });
    expect(within(stats).getByText("Reviews").parentElement).toHaveTextContent("184");
    expect(within(stats).getByText("Approved by pergent").parentElement).toHaveTextContent("121");
    expect(within(stats).getByText("Approved by pergent").parentElement).toHaveTextContent("66% of reviews");
    expect(within(stats).getByText("Blocking findings").parentElement).toHaveTextContent("37");
    expect(within(stats).getByText("Failed reviews").parentElement).toHaveTextContent("4");

    const recent = screen.getByRole("region", { name: "Recent reviews" });
    const link = within(recent).getByRole("link", { name: /acme\/payments !77/ });
    expect(link).toHaveAttribute("href", "/reviews/main/acme/payments/77");
    expect(link).toHaveTextContent("Refund idempotency key");
    expect(within(recent).getByText("Blocking")).toHaveClass("pill", "bad");
    expect(within(recent).getByText("Approved")).toHaveClass("pill", "ok");
    expect(within(recent).getByText(/security-review found blocking issues/)).toBeInTheDocument();
    expect(within(recent).getByRole("link", { name: "View all →" })).toHaveAttribute("href", "/reviews?days=7");

    expect(screen.queryByRole("region", { name: "Needs attention" })).not.toBeInTheDocument();
    expect(screen.queryByText(/needs attention/i)).not.toBeInTheDocument();
  });

  it("asks for another period", async () => {
    const fake = fakeApi({
      "GET /api/me": { body: ssoAdmin },
      "GET /api/overview?days=7": { body: overview },
      "GET /api/overview?days=30": { body: { ...overview, stats: { ...overview.stats, reviews: 999 } } },
    });
    renderApp("/");
    await screen.findByText("184");
    await userEvent.selectOptions(screen.getByRole("combobox", { name: "Period" }), "30");
    expect(await screen.findByText("999")).toBeInTheDocument();
    expect(fake.find("GET", "/api/overview?days=30")).toHaveLength(1);
  });

  it("offers Add repository only to admins", async () => {
    fakeApi({ "GET /api/me": { body: viewer }, "GET /api/overview?days=7": { body: overview } });
    renderApp("/");
    await screen.findByText("184");
    expect(screen.queryByRole("link", { name: "+ Add repository" })).not.toBeInTheDocument();
  });

  it("shows the setup checklist to an admin: done items ticked, each linking its page", async () => {
    fakeApi({
      "GET /api/me": { body: ssoAdmin },
      "GET /api/overview?days=7": {
        body: { ...overview, setup: { llm: true, instance: true, skill: false, repository: false } },
      },
    });
    renderApp("/");
    const list = await screen.findByRole("region", { name: "Setup checklist" });
    const item = (name: string) => within(list).getByRole("link", { name }).closest("li") as HTMLElement;
    expect(within(list).getByRole("link", { name: "Set the model and its key" })).toHaveAttribute("href", "/admin/settings");
    expect(within(list).getByRole("link", { name: "Connect a GitLab or GitHub instance" })).toHaveAttribute("href", "/admin/integrations");
    expect(within(list).getByRole("link", { name: "Add a skill to the library" })).toHaveAttribute("href", "/skills");
    expect(within(list).getByRole("link", { name: "Switch on a repository" })).toHaveAttribute("href", "/repos?add=1");
    expect(item("Set the model and its key")).toHaveClass("done");
    expect(within(item("Set the model and its key")).getByLabelText("done")).toBeInTheDocument();
    expect(item("Connect a GitLab or GitHub instance")).toHaveClass("done");
    expect(item("Add a skill to the library")).not.toHaveClass("done");
    expect(within(item("Add a skill to the library")).getByLabelText("to do")).toBeInTheDocument();
    expect(item("Switch on a repository")).not.toHaveClass("done");
  });

  it("has no checklist once all four are done, and none for others", async () => {
    fakeApi({ "GET /api/me": { body: ssoAdmin }, "GET /api/overview?days=7": { body: { ...overview, setup: null } } });
    renderApp("/");
    await screen.findByText("184");
    expect(screen.queryByRole("region", { name: "Setup checklist" })).not.toBeInTheDocument();
  });

  it("says when there are no reviews yet", async () => {
    fakeApi({
      "GET /api/me": { body: ssoAdmin },
      "GET /api/overview?days=7": { body: { ...overview, recent: [] } },
    });
    renderApp("/");
    expect(await screen.findByText("No reviews yet.")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "+ Add repository" })).toHaveAttribute("href", "/repos?add=1");
  });
});

describe("Overview: the numbers open their reviews", () => {
  it("links each number to the reviews behind it, over the same days", async () => {
    fakeApi({ "GET /api/me": { body: ssoAdmin }, "GET /api/overview?days=7": { body: overview } });
    renderApp("/");
    expect(await screen.findByRole("link", { name: /^Failed reviews: \d+\. Open these reviews$/ })).toHaveAttribute("href", "/reviews?outcome=failed&days=7");
    expect(screen.getByRole("link", { name: /^Blocking findings: / })).toHaveAttribute("href", "/reviews?outcome=blocking&days=7");
    expect(screen.getByRole("link", { name: /^Reviews: / })).toHaveAttribute("href", "/reviews?days=7");
  });
});

describe("Overview: the period is in the address", () => {
  it("reads ?days=30, and keeps a new choice in the address", async () => {
    fakeApi({
      "GET /api/me": { body: ssoAdmin },
      "GET /api/overview?days=30": { body: overview },
      "GET /api/overview?days=90": { body: overview },
    });
    renderApp("/?days=30");
    expect(await screen.findByRole("combobox", { name: "Period" })).toHaveValue("30");
    await userEvent.selectOptions(screen.getByRole("combobox", { name: "Period" }), "90");
    expect(screen.getByTestId("location")).toHaveTextContent("/?days=90");
  });
});
