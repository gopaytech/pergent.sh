import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import type { Can, FindingView, ReviewDetailView, RunDetailView } from "../api";
import { fakeApi } from "../test/fakeApi";
import { allCan, platformAdmin, reviewDetail, runDetail } from "../test/fixtures";
import { renderApp } from "../test/render";

afterEach(() => vi.unstubAllGlobals());

const newest = reviewDetail.runs[0].id;
const older = reviewDetail.runs[1].id;

function routes(run: RunDetailView = runDetail) {
  return {
    "GET /api/me": { body: platformAdmin },
    "GET /api/reviews/main/acme/payments/77": { body: reviewDetail },
    [`GET /api/runs/${newest}`]: { body: run },
  };
}

describe("ReviewDetail", () => {
  it("writes where each skill came from: library with its version, or the repository folder", async () => {
    const run = structuredClone(runDetail);
    run.skills = [
      run.skills[0],
      { ...run.skills[1], name: "agent-rules", source: ".claude/skills", version: 0 },
      { ...run.skills[1], name: "older", source: "repo", version: 0 },
      { ...run.skills[1], name: "unversioned", source: "library", version: 0 },
    ];
    renderAndWait(run);
    const skills = await screen.findByRole("region", { name: "Skills" });
    const row = (name: string) => within(skills).getByText(name).closest(".verdict") as HTMLElement;
    expect(row("security-review")).toHaveTextContent("library v3");
    expect(row("agent-rules")).toHaveTextContent("repo (.claude/skills)");
    expect(row("older")).toHaveTextContent(/· repo$/);
    expect(row("unversioned")).toHaveTextContent(/· library$/);
  });

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
    expect(run).toHaveTextContent("model claude-sonnet · max steps 40");
    expect(run).toHaveTextContent("Tokens: 48.2k in · 6.1k out");
    expect(run).toHaveTextContent("Files: 2 reviewed · 1 skipped · 1 not reviewed");

    const skills = screen.getByRole("region", { name: "Skills" });
    expect(within(skills).getByText("security-review").closest(".verdict")).toHaveTextContent("1 blocking");
    expect(within(skills).getByText("security-review").closest(".verdict")).toHaveTextContent("library v3");
    expect(within(skills).getByText("code-review").closest(".verdict")).toHaveTextContent("No blocking");
    expect(within(skills).getByText("code-review").closest(".verdict")).toHaveTextContent("source not recorded");
  });

  it("says the step limit was not recorded for an older run", async () => {
    renderAndWait({ ...runDetail, maxTurns: null });
    const run = await screen.findByRole("region", { name: "This run" });
    expect(await within(run).findByText("v0.1.0")).toBeInTheDocument();
    expect(run).toHaveTextContent("model claude-sonnet · max steps not recorded");
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
    // The session may do nothing to this review: no action buttons.
    expect(screen.queryByRole("button", { name: /Re-run|Dismiss|Restore/ })).not.toBeInTheDocument();
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
    fakeApi({ "GET /api/me": { body: platformAdmin } });
    renderApp("/reviews/main/acme/secret/9");
    expect(await screen.findByRole("heading", { name: "Not found" })).toBeInTheDocument();
  });

  it("shows not found for a path without a number", async () => {
    const fake = fakeApi({ "GET /api/me": { body: platformAdmin } });
    renderApp("/reviews/main/acme/payments/abc");
    expect(await screen.findByRole("heading", { name: "Not found" })).toBeInTheDocument();
    expect(fake.calls.filter((c) => c.url.startsWith("/api/reviews"))).toHaveLength(0);
  });

  it("shows not found for a number longer than 15 digits", async () => {
    const fake = fakeApi({ "GET /api/me": { body: platformAdmin } });
    renderApp("/reviews/main/acme/payments/1234567890123456");
    expect(await screen.findByRole("heading", { name: "Not found" })).toBeInTheDocument();
    expect(fake.calls.filter((c) => c.url.startsWith("/api/reviews"))).toHaveLength(0);
  });

  it("reads a project with nested groups and odd characters", async () => {
    const fake = fakeApi({
      "GET /api/me": { body: platformAdmin },
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

const rerunUrl = "/api/reviews/main/acme/payments/77/rerun";
const dismissUrl = "/api/reviews/main/acme/payments/77/dismiss";
const restoreUrl = "/api/reviews/main/acme/payments/77/restore";

const withCan = (can: Can): ReviewDetailView => ({ ...reviewDetail, role: "maintainer", can });

// dismissedRun is runDetail with the suggestion set aside.
function dismissedRun(dismissed: Partial<FindingView> = {}): RunDetailView {
  const sugg = runDetail.skills[1];
  return {
    ...runDetail,
    skills: [
      runDetail.skills[0],
      {
        ...sugg,
        findings: [],
        dismissed: [
          {
            ...sugg.findings[0],
            dismissed: { by: "maint-user (gitlab main)", reason: "intended: the window is a product rule", at: "2026-11-01T10:00:00Z" },
            ...dismissed,
          },
        ],
      },
    ],
  };
}

function renderAppAt() {
  return renderApp("/reviews/main/acme/payments/77");
}

function renderAndWait(run: RunDetailView = runDetail) {
  fakeApi(routes(run));
  return renderAppAt();
}

describe("ReviewDetail buttons", () => {
  const base = (can: Can, run: RunDetailView = runDetail) => ({
    ...routes(run),
    "GET /api/reviews/main/acme/payments/77": { body: withCan(can) },
  });

  it("shows the re-run buttons only when the session may re-run", async () => {
    fakeApi(base({ ...allCan, rerun: false }));
    renderAppAt();
    await screen.findByRole("heading", { name: /Refund idempotency key/ });
    expect(screen.queryByRole("button", { name: "Re-run review" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Re-run failed only" })).not.toBeInTheDocument();
  });

  it("re-runs the whole review and says it is queued, then reads the review and run again", async () => {
    const fake = fakeApi({ ...base(allCan), ["POST " + rerunUrl]: { status: 202, body: { status: "queued" } } });
    renderAppAt();
    await userEvent.click(await screen.findByRole("button", { name: "Re-run review" }));
    const [post] = fake.find("POST", rerunUrl);
    expect(post.headers.get("X-CSRF-Token")).toBe("csrf-123");
    expect(post.body).toEqual({ failed: false });
    expect(await screen.findByText("Re-run queued.")).toHaveAttribute("role", "status");
    await waitFor(() => expect(fake.find("GET", "/api/reviews/main/acme/payments/77")).toHaveLength(2));
    await waitFor(() => expect(fake.find("GET", `/api/runs/${newest}`).length).toBeGreaterThanOrEqual(2));
  });

  it("re-runs only the failed skills", async () => {
    const fake = fakeApi({ ...base(allCan), ["POST " + rerunUrl]: { status: 202, body: { status: "queued" } } });
    renderAppAt();
    await userEvent.click(await screen.findByRole("button", { name: "Re-run failed only" }));
    expect(fake.find("POST", rerunUrl)[0].body).toEqual({ failed: true });
  });

  it.each([
    ["already queued", "A re-run of this change is already queued."],
    ["nothing failed", "Nothing failed, so nothing was re-run."],
    ["whole review", "The whole review failed, so all of it is re-run."],
  ])("explains the answer %s", async (status, text) => {
    fakeApi({ ...base(allCan), ["POST " + rerunUrl]: { status: 202, body: { status } } });
    renderAppAt();
    await userEvent.click(await screen.findByRole("button", { name: "Re-run failed only" }));
    expect(await screen.findByText(text)).toBeInTheDocument();
  });

  it("shows the server's text at the re-run cap", async () => {
    const fake = fakeApi({
      ...base(allCan),
      ["POST " + rerunUrl]: { status: 429, body: { error: "re-run limit reached for this merge request; it resets at 00:00 UTC" } },
    });
    renderAppAt();
    await userEvent.click(await screen.findByRole("button", { name: "Re-run review" }));
    expect(await screen.findByRole("alert")).toHaveTextContent(
      "re-run limit reached for this merge request; it resets at 00:00 UTC",
    );
    expect(screen.queryByText("Re-run queued.")).not.toBeInTheDocument();
    // Nothing was queued, so nothing is read again.
    expect(fake.find("GET", "/api/reviews/main/acme/payments/77")).toHaveLength(1);
  });

  it("shows a closed change's refusal as text", async () => {
    fakeApi({ ...base(allCan), ["POST " + rerunUrl]: { status: 409, body: { error: "this change is not open" } } });
    renderAppAt();
    await userEvent.click(await screen.findByRole("button", { name: "Re-run review" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("this change is not open");
  });

  it("offers Dismiss per finding by its severity", async () => {
    fakeApi(base({ ...allCan, dismissBlocking: false }));
    renderAppAt();
    const blocking = (await screen.findByText("Refund can be applied twice on retry")).closest(".finding") as HTMLElement;
    const suggestion = screen.getByText("Name the magic retry window").closest(".finding") as HTMLElement;
    expect(within(blocking).queryByRole("button", { name: "Dismiss" })).not.toBeInTheDocument();
    expect(within(suggestion).getByRole("button", { name: "Dismiss" })).toBeInTheDocument();
  });

  it("offers no Dismiss for a suggestion when the session may not", async () => {
    fakeApi(base({ ...allCan, dismissSuggestion: false }));
    renderAppAt();
    const blocking = (await screen.findByText("Refund can be applied twice on retry")).closest(".finding") as HTMLElement;
    const suggestion = screen.getByText("Name the magic retry window").closest(".finding") as HTMLElement;
    expect(within(blocking).getByRole("button", { name: "Dismiss" })).toBeInTheDocument();
    expect(within(suggestion).queryByRole("button", { name: "Dismiss" })).not.toBeInTheDocument();
  });

  it("won't dismiss a blocking finding without a reason", async () => {
    const fake = fakeApi({ ...base(allCan), ["POST " + dismissUrl]: { body: { status: "dismissed" } } });
    renderAppAt();
    const finding = (await screen.findByText("Refund can be applied twice on retry")).closest(".finding") as HTMLElement;
    await userEvent.click(within(finding).getByRole("button", { name: "Dismiss" }));
    await userEvent.click(within(finding).getByRole("button", { name: "Dismiss finding" }));
    expect(within(finding).getByText("Dismissing a blocking finding needs a reason.")).toBeInTheDocument();
    // A reason of only spaces is none.
    await userEvent.type(within(finding).getByRole("textbox", { name: "Reason" }), "   ");
    await userEvent.click(within(finding).getByRole("button", { name: "Dismiss finding" }));
    expect(fake.find("POST", dismissUrl)).toHaveLength(0);
  });

  it("dismisses a blocking finding with its reason and reads the review again", async () => {
    const fake = fakeApi({ ...base(allCan), ["POST " + dismissUrl]: { body: { status: "dismissed" } } });
    renderAppAt();
    const finding = (await screen.findByText("Refund can be applied twice on retry")).closest(".finding") as HTMLElement;
    await userEvent.click(within(finding).getByRole("button", { name: "Dismiss" }));
    await userEvent.type(within(finding).getByRole("textbox", { name: "Reason" }), " the key is checked upstream ");
    await userEvent.click(within(finding).getByRole("button", { name: "Dismiss finding" }));
    const [post] = fake.find("POST", dismissUrl);
    expect(post.headers.get("X-CSRF-Token")).toBe("csrf-123");
    expect(post.body).toEqual({ fingerprint: "fp-refund-twice", reason: "the key is checked upstream" });
    await waitFor(() => expect(fake.find("GET", "/api/reviews/main/acme/payments/77")).toHaveLength(2));
    await waitFor(() => expect(fake.find("GET", `/api/runs/${newest}`).length).toBeGreaterThanOrEqual(2));
  });

  it("dismisses a suggestion without a reason", async () => {
    const fake = fakeApi({ ...base(allCan), ["POST " + dismissUrl]: { body: { status: "dismissed" } } });
    renderAppAt();
    const finding = (await screen.findByText("Name the magic retry window")).closest(".finding") as HTMLElement;
    await userEvent.click(within(finding).getByRole("button", { name: "Dismiss" }));
    await userEvent.click(within(finding).getByRole("button", { name: "Dismiss finding" }));
    expect(fake.find("POST", dismissUrl)[0].body).toEqual({ fingerprint: "fp-magic-window", reason: "" });
  });

  it("can cancel a dismissal", async () => {
    const fake = fakeApi(base(allCan));
    renderAppAt();
    const finding = (await screen.findByText("Name the magic retry window")).closest(".finding") as HTMLElement;
    await userEvent.click(within(finding).getByRole("button", { name: "Dismiss" }));
    await userEvent.click(within(finding).getByRole("button", { name: "Cancel" }));
    expect(within(finding).queryByRole("textbox", { name: "Reason" })).not.toBeInTheDocument();
    expect(within(finding).getByRole("button", { name: "Dismiss" })).toBeInTheDocument();
    expect(fake.calls.filter((c) => c.method === "POST")).toHaveLength(0);
  });

  it("shows the server's refusal of a dismissal as text", async () => {
    fakeApi({
      ...base(allCan),
      ["POST " + dismissUrl]: { status: 403, body: { error: "the author of a change can't dismiss its blocking findings" } },
    });
    renderAppAt();
    const finding = (await screen.findByText("Refund can be applied twice on retry")).closest(".finding") as HTMLElement;
    await userEvent.click(within(finding).getByRole("button", { name: "Dismiss" }));
    await userEvent.type(within(finding).getByRole("textbox", { name: "Reason" }), "fine");
    await userEvent.click(within(finding).getByRole("button", { name: "Dismiss finding" }));
    expect(await within(finding).findByRole("alert")).toHaveTextContent("the author of a change can't dismiss its blocking findings");
  });

  it("shows dismissed findings in a group per skill, with who and why", async () => {
    fakeApi(base(allCan, dismissedRun()));
    renderAppAt();
    expect(await screen.findByRole("tab", { name: "Findings (1)" })).toBeInTheDocument();
    const group = screen.getByRole("group", { name: "Dismissed in code-review" });
    expect(group).toHaveTextContent("Dismissed in code-review (1)");
    expect(group).toHaveTextContent("Name the magic retry window");
    expect(group).toHaveTextContent("maint-user (gitlab main)");
    expect(group).toHaveTextContent("intended: the window is a product rule");
    expect(within(group).getByRole("button", { name: "Restore" })).toBeInTheDocument();
    // A dismissed finding has no Dismiss button, and a skill with none has no group.
    expect(within(group).queryByRole("button", { name: "Dismiss" })).not.toBeInTheDocument();
    expect(screen.queryByRole("group", { name: "Dismissed in security-review" })).not.toBeInTheDocument();
    expect(screen.getByRole("region", { name: "Skills" })).toHaveTextContent("1 dismissed");
  });

  it("hides Restore from a session that may not restore", async () => {
    fakeApi(base({ ...allCan, restore: false }, dismissedRun()));
    renderAppAt();
    const group = await screen.findByRole("group", { name: "Dismissed in code-review" });
    expect(within(group).queryByRole("button", { name: "Restore" })).not.toBeInTheDocument();
  });

  it("shows the dismissed findings even when none is left standing", async () => {
    const run = dismissedRun();
    run.skills[0] = { ...run.skills[0], findings: [], verdict: "no_blocking_issues" };
    fakeApi(base(allCan, run));
    renderAppAt();
    expect(await screen.findByText("No findings.")).toBeInTheDocument();
    expect(screen.getByRole("group", { name: "Dismissed in code-review" })).toBeInTheDocument();
  });

  it("restores a dismissed finding and reads the review again", async () => {
    const fake = fakeApi({ ...base(allCan, dismissedRun()), ["POST " + restoreUrl]: { body: { status: "restored" } } });
    renderAppAt();
    const group = await screen.findByRole("group", { name: "Dismissed in code-review" });
    await userEvent.click(within(group).getByRole("button", { name: "Restore" }));
    const [post] = fake.find("POST", restoreUrl);
    expect(post.headers.get("X-CSRF-Token")).toBe("csrf-123");
    expect(post.body).toEqual({ fingerprint: "fp-magic-window" });
    await waitFor(() => expect(fake.find("GET", "/api/reviews/main/acme/payments/77")).toHaveLength(2));
    await waitFor(() => expect(fake.find("GET", `/api/runs/${newest}`).length).toBeGreaterThanOrEqual(2));
  });

  it("shows the server's refusal of a restore as text", async () => {
    fakeApi({
      ...base(allCan, dismissedRun()),
      ["POST " + restoreUrl]: { status: 500, body: { error: "the change was saved, but the review could not be decided again yet" } },
    });
    renderAppAt();
    const group = await screen.findByRole("group", { name: "Dismissed in code-review" });
    await userEvent.click(within(group).getByRole("button", { name: "Restore" }));
    expect(await within(group).findByRole("alert")).toHaveTextContent("the change was saved, but the review");
  });

  it("renders the dismissal's author and reason as text, never as HTML", async () => {
    const evil = "<img src=x onerror=alert(1)>";
    fakeApi(base(allCan, dismissedRun({ dismissed: { by: evil, reason: evil, at: "2026-11-01T10:00:00Z" } })));
    const { container } = renderAppAt();
    const group = await screen.findByRole("group", { name: "Dismissed in code-review" });
    expect(group.textContent).toContain(evil);
    expect(container.querySelector("img")).toBeNull();
  });

  it("offers the finding buttons on the run the server acts on, not by its place in the history", async () => {
    // The newest run is still running; the one before it is the latest decided.
    fakeApi({
      ...base(allCan),
      [`GET /api/runs/${newest}`]: { body: { ...runDetail, skills: [], latestDecided: false } },
      [`GET /api/runs/${older}`]: { body: { ...runDetail, run: reviewDetail.runs[1], latestDecided: true } },
    });
    renderApp(`/reviews/main/acme/payments/77?run=${older}`);
    const finding = (await screen.findByText("Refund can be applied twice on retry")).closest(".finding") as HTMLElement;
    expect(within(finding).getByRole("button", { name: "Dismiss" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Re-run review" })).toBeInTheDocument();
  });

  it("offers no finding buttons on a run the server won't act on", async () => {
    // The newest run's publication failed: it shows its findings, but the
    // server decides again another run.
    fakeApi({
      ...base(allCan, { ...dismissedRun(), latestDecided: false }),
    });
    renderAppAt();
    await screen.findByRole("group", { name: "Dismissed in code-review" });
    expect(screen.getByText("Refund can be applied twice on retry")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /Dismiss|Restore/ })).not.toBeInTheDocument();
    // Re-running still works from any run.
    expect(screen.getByRole("button", { name: "Re-run review" })).toBeInTheDocument();
  });

  it("treats a finding as blocking if any report of its fingerprint in the run is", async () => {
    // code-review reports as a suggestion what security-review reports as blocking.
    const run: RunDetailView = {
      ...runDetail,
      skills: [
        runDetail.skills[0],
        {
          ...runDetail.skills[1],
          findings: [{ ...runDetail.skills[1].findings[0], fingerprint: runDetail.skills[0].findings[0].fingerprint }],
        },
      ],
    };
    // A session that may dismiss suggestions only is not offered Dismiss on either report.
    fakeApi(base({ ...allCan, dismissBlocking: false }, run));
    renderAppAt();
    await screen.findByText("Name the magic retry window");
    expect(screen.queryByRole("button", { name: "Dismiss" })).not.toBeInTheDocument();
  });

  it("needs a reason for a suggestion that is reported as blocking elsewhere", async () => {
    const run: RunDetailView = {
      ...runDetail,
      skills: [
        runDetail.skills[0],
        {
          ...runDetail.skills[1],
          findings: [{ ...runDetail.skills[1].findings[0], fingerprint: runDetail.skills[0].findings[0].fingerprint }],
        },
      ],
    };
    const fake = fakeApi({ ...base(allCan, run), ["POST " + dismissUrl]: { body: { status: "dismissed" } } });
    renderAppAt();
    const finding = (await screen.findByText("Name the magic retry window")).closest(".finding") as HTMLElement;
    await userEvent.click(within(finding).getByRole("button", { name: "Dismiss" }));
    await userEvent.click(within(finding).getByRole("button", { name: "Dismiss finding" }));
    expect(within(finding).getByText("Dismissing a blocking finding needs a reason.")).toBeInTheDocument();
    expect(fake.find("POST", dismissUrl)).toHaveLength(0);
  });

  it("offers no Dismiss or Restore for a finding with no fingerprint", async () => {
    const run = dismissedRun();
    run.skills[0] = { ...run.skills[0], findings: [{ ...run.skills[0].findings[0], fingerprint: "" }] };
    run.skills[1] = { ...run.skills[1], dismissed: [{ ...run.skills[1].dismissed[0], fingerprint: "" }] };
    fakeApi(base(allCan, run));
    renderAppAt();
    const group = await screen.findByRole("group", { name: "Dismissed in code-review" });
    expect(screen.getByText("Refund can be applied twice on retry")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /Dismiss|Restore/ })).not.toBeInTheDocument();
    expect(within(group).getByText("Name the magic retry window")).toBeInTheDocument();
  });

  it("closes the form and shows the server's status after a dismissal", async () => {
    // The answer says it was dismissed already; the review still lists the finding.
    fakeApi({ ...base(allCan), ["POST " + dismissUrl]: { body: { status: "already dismissed" } } });
    renderAppAt();
    const finding = (await screen.findByText("Name the magic retry window")).closest(".finding") as HTMLElement;
    await userEvent.click(within(finding).getByRole("button", { name: "Dismiss" }));
    await userEvent.click(within(finding).getByRole("button", { name: "Dismiss finding" }));
    expect(await screen.findByText("Finding already dismissed.")).toHaveAttribute("role", "status");
    expect(within(finding).queryByRole("textbox", { name: "Reason" })).not.toBeInTheDocument();
    expect(within(finding).getByRole("button", { name: "Dismiss" })).toBeInTheDocument();
  });

  it("shows the status after a dismissal even though the finding moves away", async () => {
    let runs = 0;
    fakeApi({
      ...base(allCan),
      ["POST " + dismissUrl]: { body: { status: "dismissed" } },
      [`GET /api/runs/${newest}`]: () => ({ body: runs++ === 0 ? runDetail : dismissedRun() }),
    });
    renderAppAt();
    const finding = (await screen.findByText("Name the magic retry window")).closest(".finding") as HTMLElement;
    await userEvent.click(within(finding).getByRole("button", { name: "Dismiss" }));
    await userEvent.click(within(finding).getByRole("button", { name: "Dismiss finding" }));
    expect(await screen.findByRole("group", { name: "Dismissed in code-review" })).toBeInTheDocument();
    expect(screen.getByText("Finding dismissed.")).toBeInTheDocument();
  });

  it("shows the status after a restore", async () => {
    fakeApi({ ...base(allCan, dismissedRun()), ["POST " + restoreUrl]: { body: { status: "not dismissed" } } });
    renderAppAt();
    const group = await screen.findByRole("group", { name: "Dismissed in code-review" });
    await userEvent.click(within(group).getByRole("button", { name: "Restore" }));
    expect(await screen.findByText("Finding not dismissed.")).toBeInTheDocument();
  });

  it("does not carry a status note over to another run", async () => {
    fakeApi({
      ...base(allCan),
      ["POST " + dismissUrl]: { body: { status: "already dismissed" } },
      [`GET /api/runs/${older}`]: { body: { ...runDetail, run: reviewDetail.runs[1], latestDecided: true } },
    });
    renderAppAt();
    const finding = (await screen.findByText("Name the magic retry window")).closest(".finding") as HTMLElement;
    await userEvent.click(within(finding).getByRole("button", { name: "Dismiss" }));
    await userEvent.click(within(finding).getByRole("button", { name: "Dismiss finding" }));
    expect(await screen.findByText("Finding already dismissed.")).toBeInTheDocument();

    const history = screen.getByRole("region", { name: "Review history" });
    await userEvent.click(within(history).getByText("5c4b3a2"));
    expect(await screen.findByText("Refund can be applied twice on retry")).toBeInTheDocument();
    expect(screen.queryByText("Finding already dismissed.")).not.toBeInTheDocument();
  });

  it("lists findings that share an ID without a duplicate-key warning", async () => {
    const warn = vi.spyOn(console, "error").mockImplementation(() => {});
    const skill = runDetail.skills[0];
    const twin = { ...skill.findings[0] };
    const dup = dismissedRun();
    const run: RunDetailView = {
      ...dup,
      skills: [
        { ...skill, findings: [twin, { ...twin }], dismissed: [] },
        { ...dup.skills[1], dismissed: [dup.skills[1].dismissed[0], { ...dup.skills[1].dismissed[0] }] },
      ],
    };
    fakeApi(base(allCan, run));
    renderAppAt();
    expect(await screen.findAllByText(twin.title)).toHaveLength(2);
    const group = await screen.findByRole("group", { name: "Dismissed in code-review" });
    expect(within(group).getAllByRole("button", { name: "Restore" })).toHaveLength(2);
    const keyWarnings = warn.mock.calls.filter((c) => String(c[0]).includes("same key"));
    warn.mockRestore();
    expect(keyWarnings).toHaveLength(0);
  });
});
