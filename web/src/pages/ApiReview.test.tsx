import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import type { APIReviewDetail } from "../api";
import { fakeApi } from "../test/fakeApi";
import { apiReviewDone, apiReviewFailed, operator, viewer } from "../test/fixtures";
import { renderApp } from "../test/render";

afterEach(() => vi.unstubAllGlobals());

const done = apiReviewDone.id;
const failed = apiReviewFailed.id;
const page = (id: string) => `/api-clients/fastgateway/reviews/${id}`;
const api = (id: string) => `/api/admin/api-reviews/${id}`;
const routes = (view: APIReviewDetail) => ({
  "GET /api/me": { body: operator },
  [`GET ${api(view.id)}`]: { body: view },
});

describe("API review", () => {
  it("shows the subject, tenant, key, status, model and the verdict", async () => {
    fakeApi(routes(apiReviewDone));
    renderApp(page(done));
    expect(await screen.findByRole("heading", { name: "Add the retry loop" })).toBeInTheDocument();
    const facts = screen.getByRole("group", { name: "Review" });
    expect(facts).toHaveTextContent("change-1042");
    expect(facts).toHaveTextContent("payments");
    expect(facts).toHaveTextContent("ci");
    expect(facts).toHaveTextContent("claude-sonnet");
    expect(facts).toHaveTextContent("Completed");
    expect(facts).toHaveTextContent("Blocking issues");
    expect(facts).toHaveTextContent("2026-11-10 09:00 UTC");
    expect(facts).toHaveTextContent("2026-11-10 09:04 UTC");
    expect(facts).toHaveTextContent("12k in, 3.4k out");
    expect(screen.getByRole("link", { name: "fastgateway" })).toHaveAttribute("href", "/api-clients/fastgateway");
  });

  it("shows each skill's findings: file:line, severity, title, explanation, fix and suggestion as text", async () => {
    const { container } = renderPage(apiReviewDone);
    const skill = await screen.findByRole("region", { name: "Skill code-review" });
    expect(within(skill).getByText("Blocking issues")).toBeInTheDocument();
    const blocking = within(skill).getByText("The loop never stops").closest(".finding") as HTMLElement;
    expect(blocking).toHaveTextContent("internal/ledger/retry.go:42-45");
    expect(blocking).toHaveTextContent("blocking");
    expect(blocking).toHaveTextContent("It retries on every error, <b>forever</b>.");
    expect(within(blocking).getByText("Stop after 3 tries.")).toBeInTheDocument();
    expect(within(blocking).getByText("for i := 0; i < 3; i++ {").tagName).toBe("PRE");
    // Model text is text, never markup.
    expect(container.querySelector(".finding b")).toBeNull();

    const suggestion = within(skill).getByText("Name it better").closest(".finding") as HTMLElement;
    expect(suggestion).toHaveTextContent("internal/ledger/names.go");
    expect(suggestion).not.toHaveTextContent("names.go:");
    expect(suggestion).toHaveTextContent("suggestion");
    // The blocking finding comes first.
    expect(blocking.compareDocumentPosition(suggestion) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();

    const dismissed = within(skill).getByText("Old style").closest(".finding") as HTMLElement;
    expect(dismissed).toHaveTextContent("Dismissed: we keep it on purpose");
    expect(dismissed).toHaveTextContent("2026-11-09 12:00 UTC");
  });

  it("shows the reviewed files, the skipped ones with their rule and the ones not reviewed with the reason", async () => {
    renderPage(apiReviewDone);
    const skill = await screen.findByRole("region", { name: "Skill code-review" });
    const reviewed = within(skill).getByRole("group", { name: "Reviewed files" });
    expect(reviewed).toHaveTextContent("internal/ledger/retry.go");
    expect(reviewed).toHaveTextContent("internal/ledger/names.go");
    expect(within(skill).getByRole("heading", { name: "Reviewed (2)" })).toBeInTheDocument();
    const skipped = within(skill).getByRole("group", { name: "Skipped files" });
    expect(within(skipped).getByText("go.sum").closest("li")).toHaveTextContent("lockfile");
    const not = within(skill).getByRole("group", { name: "Files not reviewed" });
    expect(within(not).getByText("internal/ledger/big.go").closest("li")).toHaveTextContent("context overflow");
  });

  it("says Failed at <step>: <reason> for a failed review and shows the admin's detail as text", async () => {
    const { container } = renderPage(apiReviewFailed);
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent("Failed at starting: the checkout failed");
    const detail = screen.getByText(/fatal: bad object/);
    expect(detail.tagName).toBe("PRE");
    expect(detail).toHaveTextContent("<b>abc</b>");
    expect(container.querySelector("pre b")).toBeNull();
    // No title: the subject names the page; the review never started, so it has no model.
    expect(screen.getByRole("heading", { name: "change-1044" })).toBeInTheDocument();
    expect(screen.getByRole("group", { name: "Review" })).toHaveTextContent("Failed");
    expect(screen.queryByRole("region", { name: /^Skill / })).not.toBeInTheDocument();
  });

  it("lists each callback attempt with its time, what the URL answered and how long it took", async () => {
    const view: APIReviewDetail = {
      ...apiReviewDone,
      callback: {
        state: "pending",
        attempts: [
          { at: "2026-11-10T09:04:10Z", statusCode: 0, error: "dial tcp: connection <b>refused</b>", durationMs: 1200 },
          { at: "2026-11-10T09:04:20Z", statusCode: 502, error: "", durationMs: 80 },
        ],
      },
    };
    renderPage(view);
    const section = await screen.findByRole("region", { name: "Callback" });
    expect(section).toHaveTextContent("Will be tried again");
    const rows = within(section).getAllByRole("listitem");
    expect(rows).toHaveLength(2);
    expect(rows[0]).toHaveTextContent("2026-11-10 09:04 UTC");
    expect(rows[0]).toHaveTextContent("No answer: dial tcp: connection <b>refused</b>");
    expect(rows[0]).toHaveTextContent("1200 ms");
    expect(rows[1]).toHaveTextContent("Answered 502");
    expect(rows[1]).toHaveTextContent("80 ms");
    // Error text is text, never markup.
    expect(section.querySelector("b")).toBeNull();
  });

  it.each([
    ["delivered", "Delivered"],
    ["gave_up", "Given up"],
  ] as const)("says a callback is %s", async (state, text) => {
    const attempts = [{ at: "2026-11-10T09:04:10Z", statusCode: 200, error: "", durationMs: 30 }];
    renderPage({ ...apiReviewDone, callback: { state, attempts } });
    expect(await screen.findByRole("region", { name: "Callback" })).toHaveTextContent(text);
  });

  it("shows no callback section when the review has none", async () => {
    renderPage(apiReviewDone);
    await screen.findByRole("heading", { name: "Add the retry loop" });
    expect(screen.queryByRole("region", { name: "Callback" })).not.toBeInTheDocument();
  });

  it("says a review with no result yet has none", async () => {
    const queued: APIReviewDetail = {
      ...apiReviewFailed,
      status: "queued",
      failure: null,
      model: null,
      finishedAt: null,
      review: { ...apiReviewFailed.review, status: "queued", verdict: undefined, failure: undefined, finished_at: undefined },
    };
    renderPage(queued);
    expect(await screen.findByText("No result yet.")).toBeInTheDocument();
    expect(screen.getByRole("group", { name: "Review" })).toHaveTextContent("Queued");
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  });

  it("reads the log and the diagnostics only on the Logs tab", async () => {
    const fake = fakeApi({
      ...routes(apiReviewFailed),
      [`GET ${api(failed)}/log`]: { body: { text: "line one\n<b>line two</b>" } },
      [`GET ${api(failed)}/diagnostics`]: { body: { cloneLog: "Authentication failed", podState: "PodScheduled: Unschedulable" } },
    });
    const { container } = renderApp(page(failed));
    await screen.findByRole("heading", { name: "change-1044" });
    expect(fake.calls.filter((c) => c.url.endsWith("/log") || c.url.endsWith("/diagnostics"))).toEqual([]);
    await userEvent.click(screen.getByRole("tab", { name: "Logs" }));
    const log = await screen.findByText(/line one/);
    expect(log.tagName).toBe("PRE");
    expect(log).toHaveTextContent("<b>line two</b>");
    expect(container.querySelector("pre b")).toBeNull();
    expect(await screen.findByText("Authentication failed")).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "Clone log" })).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "Pod state" })).toBeInTheDocument();
    expect(screen.getByText("PodScheduled: Unschedulable").tagName).toBe("PRE");
    expect(fake.find("GET", `${api(failed)}/log`)).toHaveLength(1);
    expect(fake.find("GET", `${api(failed)}/diagnostics`)).toHaveLength(1);
    // Not the merge request run routes.
    expect(fake.calls.filter((c) => c.url.startsWith("/api/runs/"))).toEqual([]);
  });

  it("says when no log was kept", async () => {
    fakeApi(routes(apiReviewDone));
    renderApp(page(done));
    await userEvent.click(await screen.findByRole("tab", { name: "Logs" }));
    expect(await screen.findByText("No log was kept for this run.")).toBeInTheDocument();
  });

  it("has no re-run button, whatever the status or the tab", async () => {
    for (const view of [apiReviewDone, apiReviewFailed]) {
      const { unmount } = renderPage(view);
      await screen.findByRole("tab", { name: "Logs" });
      expect(screen.queryByRole("button", { name: /re-run/i })).toBeNull();
      await userEvent.click(screen.getByRole("tab", { name: "Logs" }));
      expect(screen.queryByRole("button", { name: /re-run/i })).toBeNull();
      unmount();
    }
  });

  it("gives the not-found page for a review that is not there", async () => {
    fakeApi({ "GET /api/me": { body: operator } });
    renderApp(page("nope"));
    expect(await screen.findByRole("heading", { name: "Not found" })).toBeInTheDocument();
  });

  it("gives a user the not-found page and calls no admin route", async () => {
    const fake = fakeApi({ "GET /api/me": { body: viewer } });
    renderApp(page(done));
    expect(await screen.findByRole("heading", { name: "Not found" })).toBeInTheDocument();
    expect(fake.calls.filter((c) => c.url.startsWith("/api/admin/"))).toEqual([]);
  });
});

function renderPage(view: APIReviewDetail) {
  fakeApi(routes(view));
  return renderApp(page(view.id));
}
