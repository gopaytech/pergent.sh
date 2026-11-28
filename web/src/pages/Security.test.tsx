import { act, fireEvent, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import type { SecurityView } from "../api";
import { fakeApi } from "../test/fakeApi";
import {
  repoDetail,
  repoPayments,
  repoToolsPaused,
  reviewBlocking,
  securityScanComplete,
  securityScanFailed,
  securityScanPartial,
  securityScanRunning,
  securityView,
  ssoAdmin,
} from "../test/fixtures";
import { renderApp } from "../test/render";

afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

const fakeClock = { toFake: ["setTimeout", "clearTimeout", "setInterval", "clearInterval", "Date"] as const };

// click clicks under fake timers, then lets the work it starts finish.
async function click(el: HTMLElement) {
  fireEvent.click(el);
  await settle();
}

// settle lets the pending fetches and renders finish under fake timers.
async function settle() {
  for (let i = 0; i < 20; i++) {
    await act(async () => {
      await new Promise<void>((r) => setImmediate(r));
      await vi.advanceTimersByTimeAsync(1);
    });
  }
}

const sec = "/api/repositories/main/acme/payments/security";
const open = "/repos/main/acme/payments?tab=security";

const routes = (view: SecurityView, role: "admin" | "viewer" = "admin") => ({
  "GET /api/me": { body: ssoAdmin },
  "GET /api/repositories/main/acme/payments": { body: repoDetail(repoPayments, role) },
  "GET /api/reviews?project=acme%2Fpayments&days=90": { body: { items: [reviewBlocking], nextCursor: "" } },
  [`GET ${sec}`]: { body: view },
  [`GET ${sec}/scans`]: { body: { scans: [] } },
  [`GET ${sec}/findings?state=open`]: { body: { findings: [], truncated: false } },
});

const withLast = (last: SecurityView["last"]): SecurityView => ({ ...securityView, last });

describe("Security", () => {
  it("shows the counts and the last scan", async () => {
    fakeApi(routes(securityView));
    renderApp(open);
    expect(await screen.findByText("Critical 1 · High 2 · Medium 3 · Low 4 · Needs validation 2")).toBeInTheDocument();
    const card = screen.getByRole("region", { name: "Last scan" });
    expect(card).toHaveTextContent("Complete");
    expect(card).toHaveTextContent("20m 0s · 5k tokens · main · 0123456 · skill security-scan");
    expect(card).not.toHaveTextContent("Reason:");
    expect(screen.getByRole("tab", { name: "Security" })).toHaveAttribute("aria-selected", "true");
  });

  it("says so when there is no scan yet", async () => {
    fakeApi(routes(withLast(null)));
    renderApp(open);
    expect(await screen.findByText("No scan yet.")).toBeInTheDocument();
  });

  it("a partial scan shows its reason", async () => {
    fakeApi(routes(withLast(securityScanPartial)));
    renderApp(open);
    expect(await screen.findByText("Reason: timeout")).toBeInTheDocument();
    expect(screen.getByText("Partial")).toBeInTheDocument();
    // The usage came from the stream: sub-agent tokens are missing.
    expect(screen.getByText("Sub-agent tokens are missing from this count.")).toBeInTheDocument();
  });

  it("a failed scan shows its reason, as text", async () => {
    const failed = { ...securityScanFailed, reason: "the <b>Job</b> could not start" };
    fakeApi(routes(withLast(failed)));
    const { container } = renderApp(open);
    expect(await screen.findByText("Reason: the <b>Job</b> could not start")).toBeInTheDocument();
    expect(container.querySelector("b")).toBeNull();
    expect(screen.getByText("Failed")).toBeInTheDocument();
  });

  it("shows the cancelled reason as it is", async () => {
    fakeApi(routes(withLast({ ...securityScanPartial, reason: "cancelled" })));
    renderApp(open);
    expect(await screen.findByText("Reason: cancelled")).toBeInTheDocument();
  });

  it("Skill updated line", async () => {
    fakeApi(routes(withLast({ ...securityScanComplete, skillUpdated: true })));
    renderApp(open);
    expect(await screen.findByText("Skill updated: some findings may show as new")).toBeInTheDocument();
  });

  it("a viewer gets no Start scan or Cancel", async () => {
    fakeApi(routes({ ...securityView, canManage: false, canStart: false, running: securityScanRunning }, "viewer"));
    renderApp(open);
    await screen.findByText("Critical 1 · High 2 · Medium 3 · Low 4 · Needs validation 2");
    expect(screen.queryByRole("button", { name: "Start scan" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Running" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Cancel" })).not.toBeInTheDocument();
  });

  it("a paused repository shows a note, not an error, and no Start for a viewer", async () => {
    fakeApi({
      ...routes({ ...securityView, canManage: false, canStart: false }, "viewer"),
      "GET /api/repositories/main/acme/tools": { body: repoDetail(repoToolsPaused, "viewer") },
      "GET /api/repositories/main/acme/tools/security": { body: { ...securityView, canManage: false, canStart: false } },
      "GET /api/repositories/main/acme/tools/security/findings?state=open": { body: { findings: [], truncated: false } },
    });
    renderApp("/repos/main/acme/tools?tab=security");
    await screen.findByText("Critical 1 · High 2 · Medium 3 · Low 4 · Needs validation 2");
    expect(screen.queryByRole("button", { name: "Start scan" })).not.toBeInTheDocument();
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  });

  it("a maintainer of a paused repository sees Start scan off, with the reason as a note", async () => {
    fakeApi({
      ...routes(securityView),
      "GET /api/repositories/main/acme/tools": { body: repoDetail(repoToolsPaused, "maintainer") },
      "GET /api/repositories/main/acme/tools/security": { body: { ...securityView, canStart: false } },
      "GET /api/repositories/main/acme/tools/security/findings?state=open": { body: { findings: [], truncated: false } },
    });
    renderApp("/repos/main/acme/tools?tab=security");
    expect(await screen.findByText("Scans are off while the repository is paused.")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Start scan" })).toBeDisabled();
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  });

  it("Start scan off shows the reason", async () => {
    const off = "Start scan is off: an admin must set securityJobEgress.cidrs in the Helm values first";
    fakeApi(routes({ ...securityView, canStart: false, startOff: off }));
    renderApp(open);
    expect(await screen.findByText(off)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Start scan" })).toBeDisabled();
  });

  it("the confirm box shows the model, limits and last cost, and posts on confirm", async () => {
    const user = userEvent.setup();
    const fake = fakeApi({ ...routes(securityView), [`POST ${sec}/scans`]: { status: 202, body: { scan: securityScanRunning } } });
    renderApp(open);
    await user.click(await screen.findByRole("button", { name: "Start scan" }));
    const box = await screen.findByRole("dialog");
    expect(within(box).getByText("Model: Qwen")).toBeInTheDocument();
    expect(within(box).getByText("Steps, main agent: 120")).toBeInTheDocument();
    expect(within(box).getByText("Steps, sub-agents: 60")).toBeInTheDocument();
    expect(within(box).getByText("Timeout: 1h 0m")).toBeInTheDocument();
    expect(within(box).getByText("Last scan: 5k tokens in 20m 0s")).toBeInTheDocument();
    expect(fake.find("POST", `${sec}/scans`)).toHaveLength(0);
    await user.click(within(box).getByRole("button", { name: "Start scan" }));
    expect(fake.find("POST", `${sec}/scans`)).toHaveLength(1);
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  });

  it("the confirm box words steps from the skill and no earlier scan", async () => {
    const user = userEvent.setup();
    const start = { ...securityView.start, stepsFromSkill: true, maxStepsFromSkill: true, subagentMaxStepsFromSkill: true, maxSteps: 600, subagentMaxSteps: 300, lastTokens: null, lastSeconds: null };
    fakeApi(routes({ ...securityView, start }));
    renderApp(open);
    await user.click(await screen.findByRole("button", { name: "Start scan" }));
    const box = await screen.findByRole("dialog");
    expect(within(box).getByText("Steps, main agent: from the skill (600)")).toBeInTheDocument();
    expect(within(box).getByText("No earlier scan to compare.")).toBeInTheDocument();
  });

  it("a 409 shows its error in the box", async () => {
    const user = userEvent.setup();
    fakeApi({
      ...routes(securityView),
      [`POST ${sec}/scans`]: { status: 409, body: { error: "a security scan is already running", scan: securityScanRunning } },
    });
    renderApp(open);
    await user.click(await screen.findByRole("button", { name: "Start scan" }));
    const box = await screen.findByRole("dialog");
    await user.click(within(box).getByRole("button", { name: "Start scan" }));
    expect(await within(box).findByText("a security scan is already running")).toBeInTheDocument();
    expect(screen.getByRole("dialog")).toBeInTheDocument();
  });

  it("each step limit says from the skill when its own setting is unset", async () => {
    const user = userEvent.setup();
    const start = { ...securityView.start, stepsFromSkill: false, maxStepsFromSkill: false, subagentMaxStepsFromSkill: true, maxSteps: 200, subagentMaxSteps: 100 };
    fakeApi(routes({ ...securityView, start }));
    renderApp(open);
    await user.click(await screen.findByRole("button", { name: "Start scan" }));
    const box = await screen.findByRole("dialog");
    expect(within(box).getByText("Steps, main agent: 200")).toBeInTheDocument();
    expect(within(box).getByText("Steps, sub-agents: from the skill (100)")).toBeInTheDocument();
  });

  it("a 409 stays on screen when the refresh shows the running scan", async () => {
    const user = userEvent.setup();
    let posted = false;
    fakeApi({
      ...routes(securityView),
      [`GET ${sec}`]: () => ({ body: posted ? { ...securityView, canStart: false, running: securityScanRunning } : securityView }),
      [`POST ${sec}/scans`]: () => {
        posted = true;
        return { status: 409, body: { error: "a security scan is already running", scan: securityScanRunning } };
      },
    });
    renderApp(open);
    await user.click(await screen.findByRole("button", { name: "Start scan" }));
    await user.click(within(await screen.findByRole("dialog")).getByRole("button", { name: "Start scan" }));
    expect(await screen.findByRole("button", { name: "Running" })).toBeDisabled();
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(screen.getByText("a security scan is already running")).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Dismiss" }));
    expect(screen.queryByText("a security scan is already running")).not.toBeInTheDocument();
  });

  it("after a 409 the box stays closed when the running scan ends", async () => {
    vi.useFakeTimers({ toFake: [...fakeClock.toFake] });
    try {
      let posted = false;
      let reads = 0;
      fakeApi({
        ...routes(securityView),
        // After the post: one answer with the running scan, then it has ended.
        [`GET ${sec}`]: () => {
          if (posted) reads++;
          return { body: posted && reads < 2 ? { ...securityView, canStart: false, running: securityScanRunning } : securityView };
        },
        [`POST ${sec}/scans`]: () => {
          posted = true;
          return { status: 409, body: { error: "a security scan is already running", scan: securityScanRunning } };
        },
      });
      const { unmount } = renderApp(open);
      await settle();
      await click(screen.getByRole("button", { name: "Start scan" }));
      await click(within(screen.getByRole("dialog")).getByRole("button", { name: "Start scan" }));
      await settle();
      expect(screen.getByRole("button", { name: "Running" })).toBeDisabled();
      expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
      await vi.advanceTimersByTimeAsync(10_000);
      await settle();
      expect(screen.getByRole("button", { name: "Start scan" })).toBeEnabled();
      expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
      unmount();
    } finally {
      vi.useRealTimers();
    }
  });

  it("Dismiss after a 409 keeps the box closed when the running scan ends", async () => {
    vi.useFakeTimers({ toFake: [...fakeClock.toFake] });
    try {
      let posted = false;
      let ended = false;
      fakeApi({
        ...routes(securityView),
        [`GET ${sec}`]: () => ({ body: posted && !ended ? { ...securityView, canStart: false, running: securityScanRunning } : securityView }),
        [`POST ${sec}/scans`]: () => {
          posted = true;
          return { status: 409, body: { error: "a security scan is already running", scan: securityScanRunning } };
        },
      });
      const { unmount } = renderApp(open);
      await settle();
      await click(screen.getByRole("button", { name: "Start scan" }));
      await click(within(screen.getByRole("dialog")).getByRole("button", { name: "Start scan" }));
      await settle();
      await click(screen.getByRole("button", { name: "Dismiss" }));
      ended = true;
      await vi.advanceTimersByTimeAsync(10_000);
      await settle();
      expect(screen.getByRole("button", { name: "Start scan" })).toBeEnabled();
      expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
      unmount();
    } finally {
      vi.useRealTimers();
    }
  });

  it("polls while running and stops after", async () => {
    vi.useFakeTimers({ toFake: [...fakeClock.toFake] });
    try {
      let reads = 0;
      const fake = fakeApi({
        ...routes(securityView),
        [`GET ${sec}`]: () => {
          reads++;
          return { body: reads < 3 ? { ...securityView, canStart: false, running: securityScanRunning } : securityView };
        },
      });
      const gets = () => fake.find("GET", sec).length;
      const { unmount } = renderApp(open);
      await settle();
      expect(screen.getByRole("button", { name: "Running" })).toBeDisabled();
      expect(screen.getByRole("button", { name: "Cancel" })).toBeInTheDocument();
      expect(gets()).toBe(1);
      await vi.advanceTimersByTimeAsync(9_000);
      expect(gets()).toBe(1);
      await vi.advanceTimersByTimeAsync(1_000);
      expect(gets()).toBe(2);
      await vi.advanceTimersByTimeAsync(10_000);
      // The third answer has no running scan: polling stops.
      expect(gets()).toBe(3);
      await settle();
      expect(screen.getByRole("button", { name: "Start scan" })).toBeEnabled();
      expect(screen.queryByRole("button", { name: "Cancel" })).not.toBeInTheDocument();
      await vi.advanceTimersByTimeAsync(60_000);
      expect(gets()).toBe(3);
      unmount();
    } finally {
      vi.useRealTimers();
    }
  });

  it("Cancel asks first, then posts", async () => {
    const fake = fakeApi({
      ...routes({ ...securityView, canStart: false, running: securityScanRunning }),
      [`POST ${sec}/scans/${securityScanRunning.id}/cancel`]: { body: { scan: securityScanRunning } },
    });
    const confirm = vi.spyOn(window, "confirm").mockReturnValueOnce(false).mockReturnValueOnce(true);
    renderApp(open);
    const user = userEvent.setup();
    const cancel = await screen.findByRole("button", { name: "Cancel" });
    const url = `${sec}/scans/${securityScanRunning.id}/cancel`;
    await user.click(cancel);
    expect(confirm).toHaveBeenCalledTimes(1);
    expect(fake.find("POST", url)).toHaveLength(0);
    await user.click(cancel);
    expect(fake.find("POST", url)).toHaveLength(1);
  });
});
