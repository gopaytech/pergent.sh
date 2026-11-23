import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import type { Can, CIJobResult, FindingView, ReviewDetailView, RunDetailView } from "../api";
import { fakeApi } from "../test/fakeApi";
import { allCan, ssoAdmin, reviewDetail, runDetail, runKubernetes } from "../test/fixtures";
import { renderApp } from "../test/render";

afterEach(() => vi.unstubAllGlobals());

const newest = reviewDetail.runs[0].id;
const older = reviewDetail.runs[1].id;

function routes(run: RunDetailView = runDetail) {
  return {
    "GET /api/me": { body: ssoAdmin },
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

  it("says what an incremental review covered, and nothing for a full one", async () => {
    const note = "Re-reviewed 2 files · 28 carried over from `abc1234`";
    const { unmount } = renderAndWait({ ...runDetail, incrementalNote: note });
    const incremental = await screen.findByRole("region", { name: "This run" });
    // The note is markdown in the comment; the page shows it as plain text.
    expect(await within(incremental).findByText("Re-reviewed 2 files · 28 carried over from abc1234")).toBeInTheDocument();
    expect(incremental).not.toHaveTextContent("`");
    unmount();

    renderAndWait();
    const full = await screen.findByRole("region", { name: "This run" });
    expect(await within(full).findByText("v0.1.0")).toBeInTheDocument();
    expect(full).not.toHaveTextContent("Re-reviewed");
    expect(full).not.toHaveTextContent("carried over");
  });

  it("labels a quiet run quiet, and says nothing for a run that was not", async () => {
    const { unmount } = renderAndWait({ ...runDetail, quiet: true });
    const quiet = await screen.findByRole("region", { name: "This run" });
    expect(await within(quiet).findByText("quiet")).toBeInTheDocument();
    // The page still shows the checks and files a quiet comment leaves out.
    expect(quiet).toHaveTextContent("Files:");
    unmount();

    renderAndWait();
    const plain = await screen.findByRole("region", { name: "This run" });
    expect(await within(plain).findByText("v0.1.0")).toBeInTheDocument();
    expect(within(plain).queryByText("quiet")).not.toBeInTheDocument();
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
    expect(run).toHaveTextContent("model anthropic / claude-sonnet · max steps 40");
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
    expect(run).toHaveTextContent("model anthropic / claude-sonnet · max steps not recorded");
  });

  it("shows the model alone for a run that recorded no provider", async () => {
    renderAndWait({ ...runDetail, provider: null });
    const run = await screen.findByRole("region", { name: "This run" });
    expect(await within(run).findByText("v0.1.0")).toBeInTheDocument();
    expect(run).toHaveTextContent("model claude-sonnet · max steps 40");
    expect(run).not.toHaveTextContent("/ claude-sonnet");
  });

  it("says the model was not recorded when neither was", async () => {
    renderAndWait({ ...runDetail, provider: null, model: null });
    const run = await screen.findByRole("region", { name: "This run" });
    expect(await within(run).findByText("v0.1.0")).toBeInTheDocument();
    expect(run).toHaveTextContent("model not recorded · max steps 40");
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

  it("shows an admin the Job, the pod and the four kubectl commands, each with a copy button", async () => {
    const user = userEvent.setup();
    renderAndWait({ ...runDetail, kubernetes: { ...runKubernetes, detail: "0/3 nodes are available: 3 Insufficient memory.\n<b>x</b>" } });
    const block = await screen.findByRole("group", { name: "Kubernetes" });
    expect(block).toHaveTextContent("pergent-jobs");
    expect(block).toHaveTextContent("pergent-3f9a1c2b7d4e");
    expect(block).toHaveTextContent("pergent-3f9a1c2b7d4e-x7k2p");
    const commands = [
      "kubectl -n pergent-jobs get pods -l job-name=pergent-3f9a1c2b7d4e",
      "kubectl -n pergent-jobs logs pergent-3f9a1c2b7d4e-x7k2p -c checkout",
      "kubectl -n pergent-jobs logs pergent-3f9a1c2b7d4e-x7k2p -c pergent",
      "kubectl -n pergent-jobs describe pod pergent-3f9a1c2b7d4e-x7k2p",
    ];
    for (const c of commands) expect(within(block).getByText(c).tagName).toBe("CODE");
    expect(within(block).getAllByRole("button", { name: /^Copy / })).toHaveLength(4);
    await user.click(within(block).getByRole("button", { name: "Copy kubectl logs -c checkout" }));
    expect(await navigator.clipboard.readText()).toBe(commands[1]);
    expect(within(block).getByRole("button", { name: "Copied" })).toBeInTheDocument();
    // The detail is text, never HTML.
    const detail = within(block).getByText(/Insufficient memory/);
    expect(detail.tagName).toBe("PRE");
    expect(detail).toHaveTextContent("<b>x</b>");
    expect(detail.querySelector("b")).toBeNull();
    expect(block).toHaveTextContent("The Job is deleted an hour after it finishes; after that only what was kept here is left.");
  });

  it("leaves out the commands that need a pod while there is no pod", async () => {
    renderAndWait({ ...runDetail, kubernetes: { ...runKubernetes, pod: "" } });
    const block = await screen.findByRole("group", { name: "Kubernetes" });
    expect(within(block).getByText("kubectl -n pergent-jobs get pods -l job-name=pergent-3f9a1c2b7d4e")).toBeInTheDocument();
    expect(within(block).getAllByRole("button", { name: /^Copy / })).toHaveLength(1);
    expect(block).not.toHaveTextContent("logs");
    expect(block).not.toHaveTextContent("describe");
    expect(block.querySelector("pre")).toBeNull();
  });

  it("shows no Kubernetes block when the run has none", async () => {
    renderAndWait();
    await screen.findByRole("region", { name: "This run" });
    expect(await screen.findByText("v0.1.0")).toBeInTheDocument();
    expect(screen.queryByRole("group", { name: "Kubernetes" })).not.toBeInTheDocument();
    expect(screen.queryByText(/kubectl/)).not.toBeInTheDocument();
  });

  it("shows the clone log after the review log, and the pod state when the answer has one", async () => {
    const fake = fakeApi({
      ...routes(),
      [`GET /api/runs/${newest}/log`]: { body: { text: "line one" } },
      [`GET /api/runs/${newest}/diagnostics`]: {
        body: { cloneLog: "fatal: Authentication failed\n<b>x</b>", podState: "phase: Pending\nPodScheduled: Unschedulable" },
      },
    });
    const { container } = renderAppAt();
    expect(fake.find("GET", `/api/runs/${newest}/diagnostics`)).toHaveLength(0);
    await userEvent.click(await screen.findByRole("tab", { name: "Logs" }));
    const clone = await screen.findByText(/Authentication failed/);
    expect(clone.tagName).toBe("PRE");
    expect(clone).toHaveTextContent("<b>x</b>");
    expect(container.querySelector("pre b")).toBeNull();
    expect(screen.getByRole("heading", { name: "Clone log" })).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "Pod state" })).toBeInTheDocument();
    expect(screen.getByText(/PodScheduled: Unschedulable/).tagName).toBe("PRE");
    const log = screen.getByText("line one");
    expect(log.compareDocumentPosition(clone) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it("shows no pod state when the answer has none, and nothing for an empty clone log", async () => {
    fakeApi({
      ...routes(),
      [`GET /api/runs/${newest}/log`]: { body: { text: "line one" } },
      [`GET /api/runs/${newest}/diagnostics`]: { body: { cloneLog: "cloned fine" } },
    });
    const { unmount } = renderAppAt();
    await userEvent.click(await screen.findByRole("tab", { name: "Logs" }));
    expect(await screen.findByText("cloned fine")).toBeInTheDocument();
    expect(screen.queryByRole("heading", { name: "Pod state" })).not.toBeInTheDocument();
    unmount();

    fakeApi({
      ...routes(),
      [`GET /api/runs/${newest}/log`]: { body: { text: "line one" } },
      [`GET /api/runs/${newest}/diagnostics`]: { body: { cloneLog: "" } },
    });
    renderAppAt();
    await userEvent.click(await screen.findByRole("tab", { name: "Logs" }));
    expect(await screen.findByText("line one")).toBeInTheDocument();
    await waitFor(() => expect(screen.queryByRole("heading", { name: "Clone log" })).not.toBeInTheDocument());
  });

  it("shows nothing extra when no diagnostics were kept, and still the kept diagnostics when the review log is gone", async () => {
    fakeApi({
      ...routes(),
      [`GET /api/runs/${newest}/log`]: { body: { text: "line one" } },
    });
    const { unmount } = renderAppAt();
    await userEvent.click(await screen.findByRole("tab", { name: "Logs" }));
    expect(await screen.findByText("line one")).toBeInTheDocument();
    expect(screen.queryByRole("heading", { name: "Clone log" })).not.toBeInTheDocument();
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
    unmount();

    fakeApi({
      ...routes(),
      [`GET /api/runs/${newest}/diagnostics`]: { body: { cloneLog: "fatal: could not read" } },
    });
    renderAppAt();
    await userEvent.click(await screen.findByRole("tab", { name: "Logs" }));
    expect(await screen.findByText("No log was kept for this run.")).toBeInTheDocument();
    expect(await screen.findByText("fatal: could not read")).toBeInTheDocument();
  });

  it("names the chain entry the run ran on and its place in the chain", async () => {
    renderAndWait({ ...runDetail, entry: { name: "qwen3.5", position: 2, length: 3, source: "team", team: "Payments" } });
    const run = await screen.findByRole("region", { name: "This run" });
    expect(await within(run).findByText("v0.1.0")).toBeInTheDocument();
    expect(run).toHaveTextContent("Model chain: qwen3.5 · entry 2 of 3 · team Payments");
  });

  it("says nothing of a chain or attempts for a run that recorded no entry", async () => {
    renderAndWait();
    const run = await screen.findByRole("region", { name: "This run" });
    expect(await within(run).findByText("v0.1.0")).toBeInTheDocument();
    expect(run).not.toHaveTextContent("Model chain:");
    expect(within(run).queryByRole("list", { name: "Attempts" })).not.toBeInTheDocument();
  });

  it("lists each attempt of the review, with why an earlier one was dropped, linked to its run", async () => {
    const dropped = "33333333-3333-3333-3333-333333333333";
    renderAndWait({
      ...runDetail,
      entry: { name: "qwen3.5", position: 2, length: 2, source: "global", team: "" },
      attempts: [
        { runId: dropped, name: "Claude <b>Sonnet</b>", position: 1, outcome: "fell_back", reason: "quota" },
        { runId: newest, name: "qwen3.5", position: 2, outcome: "blocking", reason: "" },
      ],
    });
    const run = await screen.findByRole("region", { name: "This run" });
    const list = await within(run).findByRole("list", { name: "Attempts" });
    const items = within(list).getAllByRole("listitem");
    expect(items[0]).toHaveTextContent("Attempt 1: Claude <b>Sonnet</b> (entry 1) · out of quota, so the review started again on the next model");
    expect(items[0].querySelector("b")).toBeNull();
    expect(within(items[0]).getByRole("link", { name: /Attempt 1/ })).toHaveAttribute("href", `/reviews/main/acme/payments/77?run=${dropped}`);
    expect(items[1]).toHaveTextContent("Attempt 2: qwen3.5 (entry 2) · this run");
    expect(within(items[1]).queryByRole("link")).not.toBeInTheDocument();
    expect(run).toHaveTextContent("Model chain: qwen3.5 · entry 2 of 2 · global");
  });

  it("opens a dropped attempt, which the history leaves out", async () => {
    const dropped = "33333333-3333-3333-3333-333333333333";
    const fake = fakeApi({
      ...routes(),
      [`GET /api/runs/${dropped}`]: {
        body: {
          ...runDetail,
          run: { ...reviewDetail.runs[0], id: dropped, outcome: "fell_back", reason: "quota" },
          approved: false,
          approvalReason: "quota",
          latestDecided: false,
          skills: [],
          entry: { name: "Claude Sonnet", position: 1, length: 2, source: "global", team: "" },
        },
      },
    });
    renderApp(`/reviews/main/acme/payments/77?run=${dropped}`);
    const run = await screen.findByRole("region", { name: "This run" });
    expect(await within(run).findByText("Model chain: Claude Sonnet · entry 1 of 2 · global", { exact: false })).toBeInTheDocument();
    expect(fake.find("GET", `/api/runs/${dropped}`)).toHaveLength(1);
    expect(screen.getByText("Retried on backup")).toBeInTheDocument();
    // The reason is the class the store keeps (quota), said in words.
    expect(run).toHaveTextContent("Approved: no — out of quota");
    expect(screen.getByRole("region", { name: "Skills" })).toHaveTextContent("This run has no skill results: out of quota");
    expect(run).not.toHaveTextContent("— quota");
    // No run of the history is the one shown.
    const history = screen.getByRole("region", { name: "Review history" });
    expect(within(history).queryByRole("link", { current: "true" })).not.toBeInTheDocument();
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
    fakeApi({ "GET /api/me": { body: ssoAdmin } });
    renderApp("/reviews/main/acme/secret/9");
    expect(await screen.findByRole("heading", { name: "Not found" })).toBeInTheDocument();
  });

  it("shows not found for a path without a number", async () => {
    const fake = fakeApi({ "GET /api/me": { body: ssoAdmin } });
    renderApp("/reviews/main/acme/payments/abc");
    expect(await screen.findByRole("heading", { name: "Not found" })).toBeInTheDocument();
    expect(fake.calls.filter((c) => c.url.startsWith("/api/reviews"))).toHaveLength(0);
  });

  it("shows not found for a number longer than 15 digits", async () => {
    const fake = fakeApi({ "GET /api/me": { body: ssoAdmin } });
    renderApp("/reviews/main/acme/payments/1234567890123456");
    expect(await screen.findByRole("heading", { name: "Not found" })).toBeInTheDocument();
    expect(fake.calls.filter((c) => c.url.startsWith("/api/reviews"))).toHaveLength(0);
  });

  it("reads a project with nested groups and odd characters", async () => {
    const fake = fakeApi({
      "GET /api/me": { body: ssoAdmin },
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

  it("shows an admin's refusal at a team's own limit as text", async () => {
    const text = "every model of the chain is at the team's own limit: a maintainer of Payments can raise it on the team page";
    fakeApi({ ...base(allCan), ["POST " + rerunUrl]: { status: 409, body: { error: text } } });
    renderAppAt();
    await userEvent.click(await screen.findByRole("button", { name: "Re-run review" }));
    expect(await screen.findByRole("alert")).toHaveTextContent(text);
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

describe("ReviewDetail: CI jobs", () => {
  const jobs: CIJobResult[] = [
    { name: "test", result: "failed", read: "log", size: 2048, cut: true, blocks: true },
    { name: "scan", result: "unreadable", reason: "no permission", read: "report", size: 0, cut: false, blocks: false },
    { name: "build", result: "passed", read: "status", size: 0, cut: false, blocks: true },
  ];

  it("lists each job with its result, what was read, its size and whether it was cut", async () => {
    renderAndWait({ ...runDetail, ciJobs: jobs });
    const box = await screen.findByRole("region", { name: "CI jobs" });
    const rows = within(box).getAllByRole("listitem");
    expect(rows).toHaveLength(3);
    expect(rows[0]).toHaveTextContent("test");
    expect(rows[0]).toHaveTextContent("failed");
    expect(rows[0]).toHaveTextContent("log read, 2 KiB, cut");
    expect(rows[0]).toHaveTextContent("blocks approval");
    expect(rows[1]).toHaveTextContent("unreadable (no permission)");
    expect(rows[1]).toHaveTextContent("report read, nothing read");
    expect(rows[1]).not.toHaveTextContent("blocks approval");
    expect(rows[2]).toHaveTextContent("passed");
    expect(rows[2]).toHaveTextContent("status only");
    expect(rows[2]).not.toHaveTextContent("cut");
  });

  it("shows nothing for a run with no CI jobs", async () => {
    renderAndWait();
    await screen.findByRole("region", { name: "Skills" });
    expect(screen.queryByRole("region", { name: "CI jobs" })).not.toBeInTheDocument();
  });

  it("says a run waits for CI, with its deadline", async () => {
    renderAndWait({ ...runDetail, ciJobs: null, ciDeadline: "2026-11-18T10:30:00Z", run: { ...runDetail.run, outcome: "waiting", reason: "Waiting for CI." } });
    const box = await screen.findByRole("region", { name: "CI jobs" });
    expect(box).toHaveTextContent("Waiting for CI until 2026-11-18 10:30 UTC.");
  });
});

describe("ReviewDetail path rules", () => {
  it("shows the rule each reviewed file was checked with", async () => {
    fakeApi(routes(runDetail));
    renderApp("/reviews/main/acme/payments/77");
    await userEvent.click(await screen.findByRole("tab", { name: "Files" }));
    expect(screen.getByText("internal/refund/handler.go").closest("li")).toHaveTextContent("rule: repo:internal/**/*.go");
    expect(screen.getByText("internal/refund/key.go").closest("li")).not.toHaveTextContent("rule:");
    expect(screen.queryByText("This image does not use rules.")).not.toBeInTheDocument();
  });

  it("says when the image that reviewed does not use rules", async () => {
    const run = structuredClone(runDetail);
    run.rulesUsed = false;
    run.files.rules = [];
    fakeApi(routes(run));
    renderApp("/reviews/main/acme/payments/77");
    await userEvent.click(await screen.findByRole("tab", { name: "Files" }));
    expect(screen.getByText("This image does not use rules.")).toBeInTheDocument();
  });

  it("says why the chosen built-in rules were left out", async () => {
    const run = structuredClone(runDetail);
    run.builtInRulesNote = "The chosen built-in rules were left out: no review image can load only the chosen built-in rules yet.";
    fakeApi(routes(run));
    renderApp("/reviews/main/acme/payments/77");
    await userEvent.click(await screen.findByRole("tab", { name: "Files" }));
    expect(screen.getByText(/The chosen built-in rules were left out: no review image/)).toBeInTheDocument();
  });
});

describe("ReviewDetail: multi review", () => {
  const r1a = "11111111-0000-0000-0000-000000000001";
  const r1b = "11111111-0000-0000-0000-000000000002";
  const r2 = "22222222-0000-0000-0000-000000000001";
  const r3 = "33333333-0000-0000-0000-000000000001";
  const multiRun: RunDetailView = {
    ...runDetail,
    multiReview: {
      reviewers: [
        {
          number: 1,
          entry: "qwen",
          provider: "alibaba",
          model: "qwen3",
          state: "done",
          skipped: "",
          findings: 2,
          in: 155000,
          out: 16000,
          attempts: [
            { id: r1a, entry: "sonnet", position: 1, provider: "anthropic", model: "claude-sonnet", state: "fell_back", failure: "", step: "" },
            { id: r1b, entry: "qwen", position: 2, provider: "alibaba", model: "qwen3", state: "done", failure: "", step: "" },
          ],
        },
        {
          number: 2,
          entry: "gpt",
          provider: "openai",
          model: "gpt-5",
          state: "failed",
          skipped: "the reviewer Job ran out of memory",
          findings: 0,
          in: 20000,
          out: 2000,
          attempts: [
            { id: r2, entry: "gpt", position: 1, provider: "openai", model: "gpt-5", state: "failed", failure: "the reviewer Job ran out of memory", step: "review" },
          ],
        },
        {
          number: 3,
          entry: "haiku",
          provider: "",
          model: "",
          state: "skipped",
          skipped: "the entry is out of quota",
          findings: 0,
          in: 0,
          out: 0,
          attempts: [{ id: r3, entry: "haiku", position: 0, provider: "", model: "", state: "skipped", failure: "the entry is out of quota", step: "" }],
        },
      ],
      judge: { entry: "opus", provider: "anthropic", model: "claude-opus", noJudge: false, reason: "", attempts: 1, in: 30000, out: 3000 },
      total: { in: 205000, out: 21000 },
    },
  };
  const cells = (row: HTMLElement) => within(row).getAllByRole("cell").map((c) => c.textContent);

  it("shows one row per reviewer, the judge and the total", async () => {
    renderAndWait(multiRun);
    const section = await screen.findByRole("region", { name: "Multi review" });
    const rows = within(within(section).getByRole("table", { name: "Reviewers" })).getAllByRole("row");
    // The header, 3 reviewers, the judge and the total.
    expect(rows).toHaveLength(6);
    expect(cells(rows[1])).toEqual(["Reviewer 1", "qwen", "alibaba / qwen3", "done", "2", "155k", "16k"]);
    expect(cells(rows[2])).toEqual(["Reviewer 2", "gpt", "openai / gpt-5", "failed: the reviewer Job ran out of memory", "", "20k", "2k"]);
    expect(cells(rows[3])).toEqual(["Reviewer 3", "haiku", "", "skipped: the entry is out of quota", "", "0", "0"]);
    expect(cells(rows[4])).toEqual(["Judge", "opus", "anthropic / claude-opus", "judged", "", "30k", "3k"]);
    expect(cells(rows[5])).toEqual(["Total", "", "", "", "", "205k", "21k"]);

    const one = within(within(section).getByRole("list", { name: "Reviewer 1 attempts" })).getAllByRole("listitem");
    expect(one[0]).toHaveTextContent("Attempt 1: sonnet (entry 1) · anthropic / claude-sonnet · fell back to the next model");
    expect(one[1]).toHaveTextContent("Attempt 2: qwen (entry 2) · alibaba / qwen3 · done");
    expect(within(section).getByRole("list", { name: "Reviewer 2 attempts" })).toHaveTextContent(
      "Attempt 1: gpt (entry 1) · openai / gpt-5 · failed at review: the reviewer Job ran out of memory",
    );
    const three = within(within(section).getByRole("list", { name: "Reviewer 3 attempts" })).getByRole("listitem");
    expect(three).toHaveTextContent(/^Attempt 1: haiku · skipped: the entry is out of quota/);
    expect(three).not.toHaveTextContent("(entry");
    expect(section).not.toHaveTextContent("No judge ran on this run.");
  });

  it("shows a judge run with no judge, and how many judge runs the review has", async () => {
    const run = structuredClone(multiRun);
    run.multiReview!.judge = { entry: "", provider: "", model: "", noJudge: true, reason: "the judge failed twice", attempts: 2, in: 30000, out: 3000 };
    renderAndWait(run);
    const section = await screen.findByRole("region", { name: "Multi review" });
    const rows = within(within(section).getByRole("table", { name: "Reviewers" })).getAllByRole("row");
    expect(cells(rows[4])).toEqual(["Judge", "", "", "no judge: the judge failed twice · 2 judge runs", "", "30k", "3k"]);
  });

  it("says when no judge ran on this run", async () => {
    const run = structuredClone(multiRun);
    run.multiReview!.judge = null;
    renderAndWait(run);
    const section = await screen.findByRole("region", { name: "Multi review" });
    const rows = within(within(section).getByRole("table", { name: "Reviewers" })).getAllByRole("row");
    expect(rows).toHaveLength(5);
    expect(rows.some((r) => r.textContent?.startsWith("Judge"))).toBe(false);
    expect(section).toHaveTextContent("No judge ran on this run.");
  });

  it("opens a reviewer attempt's log and diagnostics under its run", async () => {
    const fake = fakeApi({
      ...routes(multiRun),
      [`GET /api/runs/${newest}/members/${r2}/log`]: { body: { text: "reviewer <b>2</b> log" } },
      [`GET /api/runs/${newest}/members/${r2}/diagnostics`]: { body: { cloneLog: "clone of 2" } },
    });
    const { container } = renderAppAt();
    const section = await screen.findByRole("region", { name: "Multi review" });
    expect(fake.find("GET", `/api/runs/${newest}/members/${r2}/log`)).toHaveLength(0);
    const button = within(section).getByRole("button", { name: "Log of reviewer 2, attempt 1" });
    expect(button).toHaveAttribute("aria-expanded", "false");
    await userEvent.click(button);
    expect(button).toHaveAttribute("aria-expanded", "true");
    expect(await within(section).findByText("reviewer <b>2</b> log")).toBeInTheDocument();
    expect(await within(section).findByText("clone of 2")).toBeInTheDocument();
    expect(container.querySelector("pre b")).toBeNull();
    // Only the attempt asked for is read.
    expect(fake.find("GET", `/api/runs/${newest}/members/${r1a}/log`)).toHaveLength(0);
    await userEvent.click(button);
    expect(within(section).queryByText("reviewer <b>2</b> log")).not.toBeInTheDocument();
  });

  it("says when a reviewer attempt kept no log", async () => {
    renderAndWait(multiRun);
    const section = await screen.findByRole("region", { name: "Multi review" });
    const three = within(within(section).getByRole("list", { name: "Reviewer 3 attempts" })).getByRole("listitem");
    await userEvent.click(within(three).getByRole("button", { name: "Log of reviewer 3, attempt 1" }));
    expect(await within(three).findByText("No log was kept for this reviewer attempt.")).toBeInTheDocument();
  });

  it("renders entry and model names as text, never as HTML", async () => {
    const run = structuredClone(multiRun);
    run.multiReview!.reviewers[0].entry = "<b>qwen</b>";
    run.multiReview!.reviewers[0].attempts[0].entry = "<b>sonnet</b>";
    run.multiReview!.reviewers[1].model = "<i>gpt-5</i>";
    run.multiReview!.judge!.entry = "<u>opus</u>";
    const { container } = renderAndWait(run);
    const section = await screen.findByRole("region", { name: "Multi review" });
    for (const s of ["<b>qwen</b>", "<b>sonnet</b>", "<i>gpt-5</i>", "<u>opus</u>"]) expect(section).toHaveTextContent(s);
    expect([...container.querySelectorAll("b")].some((b) => b.textContent === "qwen" || b.textContent === "sonnet")).toBe(false);
    expect(container.querySelector("i")).toBeNull();
    expect(container.querySelector("u")).toBeNull();
  });

  it("shows why multi review was off", async () => {
    renderAndWait({ ...runDetail, multiReview: { off: "the review image is too old", reviewers: [], judge: null, total: { in: 0, out: 0 } } });
    const section = await screen.findByRole("region", { name: "Multi review" });
    expect(section).toHaveTextContent("Multi review off: the review image is too old. The review ran with reviewer 1 alone.");
    expect(within(section).queryByRole("table")).not.toBeInTheDocument();
  });

  it("shows no multi review for a run that is no multi review", async () => {
    renderAndWait();
    await screen.findByRole("region", { name: "Skills" });
    expect(screen.queryByRole("region", { name: "Multi review" })).not.toBeInTheDocument();
    expect(screen.queryByRole("region", { name: "Review panel" })).not.toBeInTheDocument();
    expect(screen.queryByText(/Multi review off/)).not.toBeInTheDocument();
  });
});
