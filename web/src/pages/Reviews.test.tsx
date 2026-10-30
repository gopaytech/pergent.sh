import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { fakeApi } from "../test/fakeApi";
import { admin, repoPayments, repoWidgets, reviewApproved, reviewBlocking } from "../test/fixtures";
import { renderApp } from "../test/render";

afterEach(() => vi.unstubAllGlobals());

const base = {
  "GET /api/me": { body: admin },
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
    expect(await screen.findByText("No reviews match.")).toBeInTheDocument();
    expect(fake.find("GET", "/api/reviews?project=acme%2Fpayments&outcome=blocking&days=7")).toHaveLength(1);
    expect(screen.getByTestId("location")).toHaveTextContent("/reviews?project=acme%2Fpayments&outcome=blocking&days=7");
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
});
