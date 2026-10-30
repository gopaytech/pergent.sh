import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import type { RunDetailView } from "../api";
import { fakeApi } from "../test/fakeApi";
import { admin, reviewDetail, runDetail } from "../test/fixtures";
import { renderApp } from "../test/render";

afterEach(() => vi.unstubAllGlobals());

const newest = reviewDetail.runs[0].id;
const older = reviewDetail.runs[1].id;

function routes(run: RunDetailView = runDetail) {
  return {
    "GET /api/me": { body: admin },
    "GET /api/reviews/main/acme/payments/77": { body: reviewDetail },
    [`GET /api/runs/${newest}`]: { body: run },
  };
}

describe("ReviewDetail", () => {
  it("shows the change, its history and the newest run", async () => {
    renderAndWait();
    expect(await screen.findByRole("heading", { name: /Refund idempotency key/ })).toBeInTheDocument();
    expect(screen.getByText(/acme\/payments !77 · GitLab · feature\/refund-key → main · opened by dev-user/)).toBeInTheDocument();
    const open = screen.getByRole("link", { name: "open in GitLab ↗" });
    expect(open).toHaveAttribute("href", "https://gitlab.example.com/acme/payments/-/merge_requests/77");
    expect(open).toHaveAttribute("target", "_blank");
    expect(open).toHaveAttribute("rel", "noopener noreferrer");

    const history = screen.getByRole("region", { name: "Review history" });
    expect(history).toHaveTextContent("2 runs");
    expect(within(history).getByText("a1b2c3d")).toBeInTheDocument();
    expect(within(history).getByText(/push · .* · 3m 41s/)).toBeInTheDocument();
    expect(within(history).getByText(/The review Job ran out of memory/)).toBeInTheDocument();

    const run = await screen.findByRole("region", { name: "This run" });
    expect(await within(run).findByText("v0.1.0")).toBeInTheDocument();
    expect(run).toHaveTextContent("Status on GitLab: failed");
    expect(run).toHaveTextContent("Approved: no — Blocking issues were found.");
    expect(run).toHaveTextContent("model claude-sonnet");
    expect(run).toHaveTextContent("Tokens: 48.2k in · 6.1k out");
    expect(run).toHaveTextContent("Files: 2 reviewed · 1 skipped · 1 not reviewed");

    const skills = screen.getByRole("region", { name: "Skills" });
    expect(within(skills).getByText("security-review").closest(".verdict")).toHaveTextContent("1 blocking");
    expect(within(skills).getByText("security-review").closest(".verdict")).toHaveTextContent("library skill");
    expect(within(skills).getByText("code-review").closest(".verdict")).toHaveTextContent("No blocking");
    expect(within(skills).getByText("code-review").closest(".verdict")).toHaveTextContent("source not recorded");
  });

  it("lists the findings with their fix", async () => {
    renderAndWait();
    const tab = await screen.findByRole("tab", { name: "Findings (2)" });
    expect(tab).toHaveAttribute("aria-selected", "true");
    const finding = (await screen.findByText("Refund can be applied twice on retry")).closest(".finding") as HTMLElement;
    expect(within(finding).getByText("blocking")).toHaveClass("pill", "bad");
    expect(finding).toHaveTextContent("a1b2c3");
    expect(finding).toHaveTextContent("internal/refund/handler.go:88 · security-review");
    expect(within(finding).getByText(/checked after the ledger write/)).toHaveClass("prose");
    expect(within(finding).getByText(/if seen\(key\)/).tagName).toBe("PRE");
    // No action buttons yet: they come with the commands piece.
    expect(screen.queryByRole("button", { name: /Re-run|Dismiss/ })).not.toBeInTheDocument();
  });

  it("renders model text as text, never as HTML", async () => {
    const evil = "<img src=x onerror=alert(1)>";
    const run: RunDetailView = {
      ...runDetail,
      skills: [
        {
          ...runDetail.skills[0],
          findings: [{ ...runDetail.skills[0].findings[0], title: evil, explanation: evil, suggestedFix: evil }],
          checks: [{ batch: 1, section: "S", name: "N", status: "fail", detail: evil }],
        },
      ],
    };
    const { container } = renderAndWait(run);
    expect(await screen.findAllByText(evil)).toHaveLength(3);
    await userEvent.click(screen.getByRole("tab", { name: "Checks" }));
    expect(await screen.findByText(evil)).toBeInTheDocument();
    expect(container.querySelector("img")).toBeNull();
  });

  it("shows the checks, files and log tabs", async () => {
    const fake = fakeApi({
      ...routes(),
      [`GET /api/runs/${newest}/log`]: { body: { text: "line one\n<b>line two</b>" } },
    });
    const { container } = renderAppAt();
    await userEvent.click(await screen.findByRole("tab", { name: "Checks" }));
    expect(await screen.findByText("Idempotent writes")).toBeInTheDocument();
    expect(screen.getByText("fail")).toHaveClass("pill");

    await userEvent.click(screen.getByRole("tab", { name: "Files" }));
    expect(screen.getByText("internal/refund/key.go")).toBeInTheDocument();
    expect(screen.getByText("go.sum").closest("li")).toHaveTextContent("lockfile");
    expect(screen.getByText("internal/ledger/big.go").closest("li")).toHaveTextContent("context overflow");

    expect(fake.find("GET", `/api/runs/${newest}/log`)).toHaveLength(0);
    await userEvent.click(screen.getByRole("tab", { name: "Logs" }));
    const log = await screen.findByText(/line one/);
    expect(log.tagName).toBe("PRE");
    expect(log).toHaveTextContent("<b>line two</b>");
    expect(container.querySelector("pre b")).toBeNull();
  });

  it("says when no log was kept", async () => {
    fakeApi(routes());
    renderAppAt();
    await userEvent.click(await screen.findByRole("tab", { name: "Logs" }));
    expect(await screen.findByText("No log was kept for this run.")).toBeInTheDocument();
  });

  it("selects an older run", async () => {
    const fake = fakeApi({
      ...routes(),
      [`GET /api/runs/${older}`]: {
        body: { ...runDetail, run: reviewDetail.runs[1], status: "failed", skills: [], approvalReason: "The review failed." },
      },
    });
    renderAppAt();
    const history = await screen.findByRole("region", { name: "Review history" });
    await userEvent.click(within(history).getByText("5c4b3a2"));
    expect(await screen.findByText(/Approved: no — The review failed\./)).toBeInTheDocument();
    expect(fake.find("GET", `/api/runs/${older}`)).toHaveLength(1);
    expect(screen.getByTestId("location")).toHaveTextContent(`?run=${older}`);
    expect(screen.getByText(/This run has no skill results/)).toBeInTheDocument();
  });

  it("links only to http(s) addresses", async () => {
    fakeApi({
      ...routes(),
      "GET /api/reviews/main/acme/payments/77": {
        body: { ...reviewDetail, header: { ...reviewDetail.header, webURL: "javascript:alert(1)" } },
      },
    });
    renderAppAt();
    await screen.findByRole("heading", { name: /Refund idempotency key/ });
    expect(screen.queryByRole("link", { name: /open in GitLab/ })).not.toBeInTheDocument();
  });

  it("shows not found for a review the user can't see", async () => {
    fakeApi({ "GET /api/me": { body: admin } });
    renderApp("/reviews/main/acme/secret/9");
    expect(await screen.findByRole("heading", { name: "Not found" })).toBeInTheDocument();
  });

  it("shows not found for a path without a number", async () => {
    const fake = fakeApi({ "GET /api/me": { body: admin } });
    renderApp("/reviews/main/acme/payments/abc");
    expect(await screen.findByRole("heading", { name: "Not found" })).toBeInTheDocument();
    expect(fake.calls.filter((c) => c.url.startsWith("/api/reviews"))).toHaveLength(0);
  });

  it("reads a project with nested groups and odd characters", async () => {
    const fake = fakeApi({
      "GET /api/me": { body: admin },
      "GET /api/reviews/main/acme/sub/we%20ird/5": {
        body: { ...reviewDetail, header: { ...reviewDetail.header, project: "acme/sub/we ird", number: 5 } },
      },
      [`GET /api/runs/${newest}`]: { body: runDetail },
    });
    renderApp("/reviews/main/acme/sub/we%20ird/5");
    expect(await screen.findByRole("heading", { name: /Refund idempotency key/ })).toBeInTheDocument();
    expect(fake.find("GET", "/api/reviews/main/acme/sub/we%20ird/5")).toHaveLength(1);
  });
});

function renderAppAt() {
  return renderApp("/reviews/main/acme/payments/77");
}

function renderAndWait(run: RunDetailView = runDetail) {
  fakeApi(routes(run));
  return renderAppAt();
}
