import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { fakeApi } from "../test/fakeApi";
import { admin, developer, overview } from "../test/fixtures";
import { renderApp } from "../test/render";

afterEach(() => vi.unstubAllGlobals());

describe("Overview", () => {
  it("shows the numbers, recent reviews and what needs attention", async () => {
    fakeApi({ "GET /api/me": { body: admin }, "GET /api/overview?days=7": { body: overview } });
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
    expect(within(recent).getByRole("link", { name: "View all →" })).toHaveAttribute("href", "/reviews");

    const attention = screen.getByRole("region", { name: "Needs attention" });
    expect(within(attention).getByText("acme/payments")).toBeInTheDocument();
    expect(within(attention).getByText(/One approval is enough/)).toBeInTheDocument();
    expect(within(attention).getByText("GitLab · main")).toBeInTheDocument();
    expect(within(attention).getByText("Token")).toHaveClass("pill");
  });

  it("asks for another period", async () => {
    const fake = fakeApi({
      "GET /api/me": { body: admin },
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
    fakeApi({ "GET /api/me": { body: developer }, "GET /api/overview?days=7": { body: overview } });
    renderApp("/");
    await screen.findByText("184");
    expect(screen.queryByRole("link", { name: "+ Add repository" })).not.toBeInTheDocument();
  });

  it("says when nothing needs attention", async () => {
    fakeApi({
      "GET /api/me": { body: admin },
      "GET /api/overview?days=7": { body: { ...overview, recent: [], attention: [] } },
    });
    renderApp("/");
    expect(await screen.findByText("No reviews yet.")).toBeInTheDocument();
    expect(screen.getByText("Nothing needs attention.")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "+ Add repository" })).toHaveAttribute("href", "/repos?add=1");
  });
});
