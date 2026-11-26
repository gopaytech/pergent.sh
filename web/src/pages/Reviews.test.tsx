import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { fakeApi } from "../test/fakeApi";
import { ssoAdmin, repoPayments, repoWidgets, reviewApproved, reviewBlocking } from "../test/fixtures";
import { renderApp } from "../test/render";

afterEach(() => vi.unstubAllGlobals());

const base = {
  "GET /api/me": { body: ssoAdmin },
  "GET /api/repositories": { body: [repoPayments, repoWidgets] },
};

describe("Reviews", () => {
  it("lists the reviews", async () => {
    fakeApi({ ...base, "GET /api/reviews?days=90": { body: { items: [reviewApproved, reviewBlocking], nextCursor: "" } } });
    renderApp("/reviews");
    expect(await screen.findByRole("heading", { name: "Reviews" })).toBeInTheDocument();
    const list = await screen.findByRole("region", { name: "Review list" });
    expect(within(list).getByRole("link", { name: /acme\/widgets !128/ })).toHaveAttribute(
      "href",
      "/reviews/main/acme/widgets/128",
    );
    expect(within(list).getByText("Approved")).toHaveClass("pill");
    expect(within(list).getByText(/security-review found blocking issues/)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Load more" })).not.toBeInTheDocument();
  });

  it("filters by repository, outcome and time", async () => {
    const fake = fakeApi({
      ...base,
      "GET /api/reviews?days=90": { body: { items: [reviewApproved, reviewBlocking], nextCursor: "" } },
      "GET /api/reviews?project=acme%2Fpayments&days=90": { body: { items: [reviewBlocking], nextCursor: "" } },
      "GET /api/reviews?project=acme%2Fpayments&outcome=blocking&days=90": { body: { items: [reviewBlocking], nextCursor: "" } },
      "GET /api/reviews?project=acme%2Fpayments&outcome=blocking&days=7": { body: { items: [], nextCursor: "" } },
    });
    renderApp("/reviews");
    await screen.findByRole("link", { name: /acme\/widgets !128/ });
    await userEvent.selectOptions(await screen.findByRole("combobox", { name: "Repository" }), "acme/payments");
    await userEvent.selectOptions(screen.getByRole("combobox", { name: "Result" }), "blocking");
    await userEvent.selectOptions(screen.getByRole("combobox", { name: "Time" }), "7");
    expect(await screen.findByText(/No reviews match these filters\./)).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Clear the filters" })).toHaveAttribute("href", "/reviews");
    expect(fake.find("GET", "/api/reviews?project=acme%2Fpayments&outcome=blocking&days=7")).toHaveLength(1);
    expect(screen.getByTestId("location")).toHaveTextContent("/reviews?project=acme%2Fpayments&outcome=blocking&days=7");
  });

  it("shows a skipped run as Not reviewed, never as Superseded, and filters on it", async () => {
    const skipped = {
      ...reviewApproved,
      number: 9,
      outcome: "skipped" as const,
      reason: "Not reviewed: this repository has no skills.",
    };
    const fake = fakeApi({
      ...base,
      "GET /api/reviews?days=90": { body: { items: [skipped], nextCursor: "" } },
      "GET /api/reviews?outcome=skipped&days=90": { body: { items: [skipped], nextCursor: "" } },
    });
    renderApp("/reviews");
    const list = await screen.findByRole("region", { name: "Review list" });
    expect(within(list).getByText("Not reviewed", { selector: ".pill" })).toHaveClass("grey");
    expect(within(list).queryByText("Superseded")).not.toBeInTheDocument();
    expect(within(list).getByText(/this repository has no skills/)).toBeInTheDocument();
    await userEvent.selectOptions(await screen.findByRole("combobox", { name: "Result" }), "skipped");
    await within(list).findByText("Not reviewed", { selector: ".pill" });
    expect(fake.find("GET", "/api/reviews?outcome=skipped&days=90")).toHaveLength(1);
  });

  it("loads the next page", async () => {
    fakeApi({
      ...base,
      "GET /api/reviews?days=90": { body: { items: [reviewApproved], nextCursor: "abc" } },
      "GET /api/reviews?days=90&cursor=abc": { body: { items: [reviewBlocking], nextCursor: "" } },
    });
    renderApp("/reviews");
    await userEvent.click(await screen.findByRole("button", { name: "Load more" }));
    expect(await screen.findByRole("link", { name: /acme\/payments !77/ })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: /acme\/widgets !128/ })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Load more" })).not.toBeInTheDocument();
  });

  it("says how many attempts a review took when it fell back, and nothing for one attempt", async () => {
    const fellBack = { ...reviewBlocking, attempts: 2 };
    fakeApi({ ...base, "GET /api/reviews?days=90": { body: { items: [reviewApproved, fellBack], nextCursor: "" } } });
    renderApp("/reviews");
    const list = await screen.findByRole("region", { name: "Review list" });
    const row = within(list).getByRole("link", { name: /acme\/payments !77/ }).closest(".row") as HTMLElement;
    expect(within(row).getByText(/security-review found blocking issues\. · 2 attempts ·/)).toBeInTheDocument();
    const one = within(list).getByRole("link", { name: /acme\/widgets !128/ }).closest(".row") as HTMLElement;
    expect(one).not.toHaveTextContent(/attempt/);
  });

  it("writes a GitHub pull request with #", async () => {
    fakeApi({
      ...base,
      "GET /api/reviews?days=90": {
        body: { items: [{ ...reviewApproved, platform: "github", instance: "gh" }], nextCursor: "" },
      },
    });
    renderApp("/reviews");
    expect(await screen.findByRole("link", { name: /acme\/widgets #128/ })).toHaveAttribute(
      "href",
      "/reviews/gh/acme/widgets/128",
    );
  });

  it("filters by owner team and narrows the Repository select", async () => {
    const fake = fakeApi({
      ...base,
      "GET /api/reviews?days=90": { body: { items: [reviewApproved, reviewBlocking], nextCursor: "" } },
      "GET /api/reviews?days=90&team=1": { body: { items: [reviewBlocking], nextCursor: "" } },
    });
    renderApp("/reviews");
    await screen.findByRole("link", { name: /acme\/widgets !128/ });
    const team = await screen.findByRole("combobox", { name: "Team" });
    expect(within(team).getAllByRole("option").map((o) => o.textContent)).toEqual(["All teams", "No owner", "payments"]);
    await userEvent.selectOptions(team, "payments");
    await waitFor(() => expect(fake.find("GET", "/api/reviews?days=90&team=1")).toHaveLength(1));
    const repo = screen.getByRole("combobox", { name: "Repository" });
    expect(within(repo).getAllByRole("option").map((o) => o.textContent)).toEqual(["All repositories", "acme/payments"]);
    expect(screen.getByTestId("location")).toHaveTextContent("/reviews?team=1");
  });

  it("clears a repository the picked team does not own", async () => {
    const fake = fakeApi({
      ...base,
      "GET /api/reviews?project=acme%2Fwidgets&days=90": { body: { items: [reviewApproved], nextCursor: "" } },
      "GET /api/reviews?days=90&team=1": { body: { items: [reviewBlocking], nextCursor: "" } },
    });
    renderApp("/reviews?project=acme%2Fwidgets");
    await screen.findByRole("link", { name: /acme\/widgets !128/ });
    await userEvent.selectOptions(await screen.findByRole("combobox", { name: "Team" }), "payments");
    await waitFor(() => expect(fake.find("GET", "/api/reviews?days=90&team=1")).toHaveLength(1));
    expect(screen.getByRole("combobox", { name: "Repository" })).toHaveValue("");
    expect(screen.getByTestId("location")).toHaveTextContent("/reviews?team=1");
  });

  it("sends no team for a team that owns nothing in the list", async () => {
    const fake = fakeApi({
      ...base,
      "GET /api/reviews?days=90": { body: { items: [reviewApproved], nextCursor: "" } },
    });
    renderApp("/reviews?team=99");
    await screen.findByRole("link", { name: /acme\/widgets !128/ });
    expect(screen.getByRole("combobox", { name: "Team" })).toHaveValue("");
    expect(fake.find("GET", "/api/reviews?days=90")).toHaveLength(1);
  });

  it("sends team=none and lists only repositories with no owner", async () => {
    fakeApi({
      ...base,
      "GET /api/reviews?days=90&team=none": { body: { items: [], nextCursor: "" } },
    });
    renderApp("/reviews?team=none");
    expect(await screen.findByText(/No reviews match these filters\./)).toBeInTheDocument();
    const repo = screen.getByRole("combobox", { name: "Repository" });
    expect(within(repo).getAllByRole("option").map((o) => o.textContent)).toEqual(["All repositories", "acme/widgets"]);
    expect(screen.getByRole("link", { name: "Clear the filters" })).toHaveAttribute("href", "/reviews");
  });
});
