import { act, fireEvent, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import type { ChecklistItem, ReadinessView, ScanView } from "../api";
import { fakeApi } from "../test/fakeApi";
import { overview, platformAdmin, repoDetail, repoPayments, repoWidgets, reviewBlocking } from "../test/fixtures";
import { renderApp } from "../test/render";

afterEach(() => vi.unstubAllGlobals());

// The clock the polling tests fake. setImmediate stays real: settle() uses it
// to let the fetch answers and renders finish, and real time never moves the
// fake clock.
const fakeClock = { toFake: ["setTimeout", "clearTimeout", "setInterval", "clearInterval", "Date"] as const };

// settle lets the pending fetches and renders finish under fake timers.
async function settle() {
  for (let i = 0; i < 20; i++) {
    await act(async () => {
      await new Promise<void>((r) => setImmediate(r));
      await vi.advanceTimersByTimeAsync(1);
    });
  }
}

// waitForGone waits until the checkbox of a skill is no longer listed.
const waitForGone = (name: string) => waitFor(() => expect(screen.queryByRole("checkbox", { name })).not.toBeInTheDocument());

const daysAgo = (n: number) => new Date(Date.now() - n * 86_400_000).toISOString();

const scan = (over: Partial<ScanView> = {}): ScanView => ({
  id: "scan-1",
  state: "done",
  startedBy: "alice (gitlab main)",
  branch: "main",
  commit: "abcdef1234567890",
  error: "",
  queuedAt: daysAgo(3),
  finishedAt: daysAgo(3),
  ...over,
});

const item = (key: string, over: Partial<ChecklistItem> = {}): ChecklistItem => ({
  key,
  state: "passed",
  reason: "",
  why: "",
  fix: "",
  ...over,
});

const view = (over: Partial<ReadinessView> = {}): ReadinessView => ({
  latest: scan(),
  active: null,
  checklist: {
    sections: [
      { key: "context", items: [item("agent_file", { state: "passed", reason: "AGENTS.md found" })] },
      { key: "process", items: [item("codeowners")] },
      { key: "skills", items: [item("skill_match")] },
      { key: "size", items: [item("noise")] },
    ],
    suggested: [],
    budget: 1000,
  },
  canScan: true,
  configRepo: false,
  followsDefaults: false,
  applyBase: ["code-review"],
  ...over,
});

const noInputs = { branch: "main", skills: [], detected: [], instructions: "" };

const route = (body: ReadinessView, repo = "acme/payments") => ({
  [`GET /api/repositories/main/${repo}/readiness`]: { body },
});

const base = (readiness: ReadinessView, repo = repoPayments) => ({
  "GET /api/me": { body: platformAdmin },
  [`GET /api/repositories/main/${repo.project}`]: { body: repoDetail(repo) },
  [`GET /api/repositories/main/${repo.project}/inputs`]: { body: noInputs },
  "GET /api/reviews?project=acme%2Fpayments&days=90": { body: { items: [reviewBlocking], nextCursor: "" } },
  ...route(readiness, repo.project),
});

describe("Readiness", () => {
  it("shows Not scanned yet and Scan now for a maintainer", async () => {
    fakeApi(base(view({ latest: null, checklist: null })));
    renderApp("/repos/main/acme/payments?tab=readiness");
    expect(await screen.findByText("Not scanned yet")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Scan now" })).toBeEnabled();
  });

  it("hides Scan now from a viewer", async () => {
    fakeApi(base(view({ canScan: false })));
    renderApp("/repos/main/acme/payments?tab=readiness");
    expect(await screen.findByText(/Scanned 3 days ago, by alice \(gitlab main\), at commit abcdef1/)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Scan now" })).not.toBeInTheDocument();
  });

  it("words the top line for a switch-on and an upgrade scan", async () => {
    fakeApi(base(view({ latest: scan({ startedBy: "switch-on" }) })));
    const { unmount } = renderApp("/repos/main/acme/payments?tab=readiness");
    expect(
      await screen.findByText("Scanned 3 days ago, when the repository was switched on, at commit abcdef1"),
    ).toBeInTheDocument();
    unmount();
    fakeApi(base(view({ latest: scan({ startedBy: "upgrade" }) })));
    renderApp("/repos/main/acme/payments?tab=readiness");
    expect(await screen.findByText("Scanned 3 days ago, after the upgrade, at commit abcdef1")).toBeInTheDocument();
  });

  it("says so on the top line when the latest scan failed, with the error as text", async () => {
    fakeApi(
      base(
        view({
          latest: scan({ state: "failed", commit: "", error: "the clone took <b>longer</b> than 5 minutes" }),
          checklist: {
            sections: [{ key: "context", items: [item("readme", { state: "unknown", reason: "couldn't scan files: the clone took longer than 5 minutes" })] }],
            suggested: [],
            budget: 0,
          },
        }),
      ),
    );
    const { container } = renderApp("/repos/main/acme/payments?tab=readiness");
    expect(
      await screen.findByText("Last scan failed 3 days ago: the clone took <b>longer</b> than 5 minutes"),
    ).toBeInTheDocument();
    expect(screen.queryByText(/^Scanned /)).not.toBeInTheDocument();
    expect(container.querySelector("b")).toBeNull();
    expect(screen.getByText("Couldn't check")).toBeInTheDocument();
  });

  it("shows each state as a pill with its reason, why and fix", async () => {
    fakeApi(
      base(
        view({
          checklist: {
            sections: [
              {
                key: "context",
                items: [
                  item("agent_file", { state: "missing", reason: "no AGENTS.md", why: "Reviews guess.", fix: "Add AGENTS.md at the root." }),
                  item("readme", { state: "passed", reason: "README.md found" }),
                  item("contributing", { state: "warning", reason: "no guide", why: "Conventions matter." }),
                ],
              },
            ],
            suggested: [],
            budget: 0,
          },
        }),
      ),
    );
    renderApp("/repos/main/acme/payments?tab=readiness");
    expect(await screen.findByText("Missing")).toBeInTheDocument();
    expect(screen.getByText("Passed")).toBeInTheDocument();
    expect(screen.getByText("Warning")).toBeInTheDocument();
    expect(screen.getByText("no AGENTS.md")).toBeInTheDocument();
    expect(screen.getByText("Reviews guess.")).toBeInTheDocument();
    const fix = screen.getByText("Add AGENTS.md at the root.");
    expect(fix.tagName).toBe("PRE");
  });

  it("shows Couldn't check with the reason, never Passed, for an unknown item", async () => {
    fakeApi(
      base(
        view({
          checklist: {
            sections: [
              { key: "process", items: [item("protected", { state: "unknown", reason: "the bot can't read branch protection here" })] },
            ],
            suggested: [],
            budget: 0,
          },
        }),
      ),
    );
    renderApp("/repos/main/acme/payments?tab=readiness");
    expect(await screen.findByText("Couldn't check")).toBeInTheDocument();
    expect(screen.getByText("the bot can't read branch protection here")).toBeInTheDocument();
    expect(screen.queryByText("Passed")).not.toBeInTheDocument();
  });

  it("shows scan text as text, never as markup", async () => {
    fakeApi(
      base(
        view({
          checklist: {
            sections: [
              {
                key: "size",
                items: [
                  item("noise", {
                    state: "warning",
                    reason: "<img src=x onerror=alert(1)>",
                    noise: [{ path: "<b>bold</b>.js", why: "big", size: 600_000 }],
                  }),
                ],
              },
            ],
            suggested: [],
            budget: 0,
          },
        }),
      ),
    );
    const { container } = renderApp("/repos/main/acme/payments?tab=readiness");
    expect(await screen.findByText("<img src=x onerror=alert(1)>")).toBeInTheDocument();
    expect(screen.getByText("<b>bold</b>.js")).toBeInTheDocument();
    expect(container.querySelector("img")).toBeNull();
    expect(container.querySelector("b")).toBeNull();
    expect(container.querySelector(".card a")).toBeNull();
  });

  it("queues a scan and shows Scanning… while it runs", async () => {
    let queued = false;
    const fake = fakeApi({
      ...base(view()),
      "GET /api/repositories/main/acme/payments/readiness": () => ({
        body: queued ? view({ active: scan({ id: "s2", state: "queued", commit: "", finishedAt: null }) }) : view(),
      }),
      "POST /api/repositories/main/acme/payments/readiness/scan": () => {
        queued = true;
        return { status: 202, body: { scan: scan({ id: "s2", state: "queued", commit: "", finishedAt: null }) } };
      },
    });
    renderApp("/repos/main/acme/payments?tab=readiness");
    await userEvent.click(await screen.findByRole("button", { name: "Scan now" }));
    expect(await screen.findByText("Scanning…")).toBeInTheDocument();
    expect(fake.find("POST", "/api/repositories/main/acme/payments/readiness/scan")).toHaveLength(1);
    expect(screen.getByRole("button", { name: "Scan now" })).toBeDisabled();
  });

  it("says a scan is already running on 409 and the wait on 429", async () => {
    let status = 409;
    fakeApi({
      ...base(view()),
      "POST /api/repositories/main/acme/payments/readiness/scan": () =>
        status === 409
          ? { status: 409, body: { error: "a scan is already queued or running", scan: scan({ state: "running" }) } }
          : { status: 429, body: { error: "too soon", waitSeconds: 181 } },
    });
    renderApp("/repos/main/acme/payments?tab=readiness");
    await userEvent.click(await screen.findByRole("button", { name: "Scan now" }));
    expect(await screen.findByText("A scan is already running.")).toBeInTheDocument();
    status = 429;
    await userEvent.click(screen.getByRole("button", { name: "Scan now" }));
    // 181 seconds rounds up to 4 minutes.
    expect(await screen.findByText("Try again in 4 minutes.")).toBeInTheDocument();
    expect(screen.queryByText("A scan is already running.")).not.toBeInTheDocument();
  });

  it("writes 1 minute, not 1 minutes", async () => {
    fakeApi({
      ...base(view()),
      "POST /api/repositories/main/acme/payments/readiness/scan": { status: 429, body: { error: "too soon", waitSeconds: 20 } },
    });
    renderApp("/repos/main/acme/payments?tab=readiness");
    await userEvent.click(await screen.findByRole("button", { name: "Scan now" }));
    expect(await screen.findByText("Try again in 1 minute.")).toBeInTheDocument();
  });

  // Under fake timers the clock moves only when the test moves it, so the
  // fetch counts below do not depend on how fast the machine is. Testing
  // Library's waitFor needs real timers, so these tests flush with settle().
  it("polls every 5 seconds only while a scan is active, and stops on unmount", async () => {
    vi.useFakeTimers({ toFake: [...fakeClock.toFake] });
    try {
      let reads = 0;
      const fake = fakeApi({
        ...base(view()),
        "GET /api/repositories/main/acme/payments/readiness": () => {
          reads++;
          return { body: reads < 3 ? view({ active: scan({ state: "running", finishedAt: null }) }) : view() };
        },
      });
      const gets = () => fake.find("GET", "/api/repositories/main/acme/payments/readiness").length;
      const { unmount } = renderApp("/repos/main/acme/payments?tab=readiness");
      await settle();
      expect(screen.getByText("Scanning…")).toBeInTheDocument();
      expect(gets()).toBe(1);
      await vi.advanceTimersByTimeAsync(4_000);
      expect(gets()).toBe(1);
      await vi.advanceTimersByTimeAsync(1_000);
      expect(gets()).toBe(2);
      await vi.advanceTimersByTimeAsync(5_000);
      // The third answer has no active scan: polling stops.
      expect(gets()).toBe(3);
      await settle();
      expect(screen.getByText(/Scanned 3 days ago/)).toBeInTheDocument();
      expect(screen.queryByText("Scanning…")).not.toBeInTheDocument();
      await vi.advanceTimersByTimeAsync(60_000);
      expect(gets()).toBe(3);
      unmount();
    } finally {
      vi.useRealTimers();
    }
  });

  it("stops polling when the tab is left", async () => {
    vi.useFakeTimers({ toFake: [...fakeClock.toFake] });
    try {
      const fake = fakeApi({
        ...base(view({ active: scan({ state: "running", finishedAt: null }) })),
      });
      const gets = () => fake.find("GET", "/api/repositories/main/acme/payments/readiness").length;
      renderApp("/repos/main/acme/payments?tab=readiness");
      await settle();
      expect(screen.getByText("Scanning…")).toBeInTheDocument();
      expect(gets()).toBe(1);
      await vi.advanceTimersByTimeAsync(5_000);
      expect(gets()).toBe(2);
      // userEvent waits on a real setTimeout in Testing Library's asyncWrapper and
      // would hang on the fake clock, so the click is a plain fireEvent.
      fireEvent.click(screen.getByRole("tab", { name: "Reviews" }));
      await settle();
      await vi.advanceTimersByTimeAsync(60_000);
      expect(gets()).toBe(2);
    } finally {
      vi.useRealTimers();
    }
  });

  it("confirms before leaving the default list, then PUTs applyBase plus the ticked skills", async () => {
    const confirm = vi.spyOn(window, "confirm").mockReturnValue(true);
    const fake = fakeApi({
      ...base(
        view({
          followsDefaults: true,
          applyBase: ["code-review"],
          checklist: { sections: [], suggested: ["security-review", "go-style"], budget: 0 },
        }),
      ),
      "PUT /api/repositories/main/acme/payments/settings": { body: repoDetail(repoPayments) },
    });
    renderApp("/repos/main/acme/payments?tab=readiness");
    const button = await screen.findByRole("button", { name: "Use these skills" });
    expect(button).toBeDisabled();
    await userEvent.click(screen.getByRole("checkbox", { name: "security-review" }));
    await userEvent.click(button);
    expect(confirm).toHaveBeenCalledWith("This repository will stop following the default skill list.");
    const puts = fake.find("PUT", "/api/repositories/main/acme/payments/settings");
    expect(puts).toHaveLength(1);
    expect(puts[0].body).toEqual({ skills: ["code-review", "security-review"] });
    expect(puts[0].headers.get("X-CSRF-Token")).toBe(platformAdmin.csrfToken);
    confirm.mockRestore();
  });

  it("does not save when the confirm is refused", async () => {
    const confirm = vi.spyOn(window, "confirm").mockReturnValue(false);
    const fake = fakeApi({
      ...base(view({ followsDefaults: true, checklist: { sections: [], suggested: ["go-style"], budget: 0 } })),
      "PUT /api/repositories/main/acme/payments/settings": { body: repoDetail(repoPayments) },
    });
    renderApp("/repos/main/acme/payments?tab=readiness");
    await userEvent.click(await screen.findByRole("checkbox", { name: "go-style" }));
    await userEvent.click(screen.getByRole("button", { name: "Use these skills" }));
    expect(confirm).toHaveBeenCalledTimes(1);
    expect(fake.find("PUT", "/api/repositories/main/acme/payments/settings")).toHaveLength(0);
    confirm.mockRestore();
  });

  it("does not ask to confirm for a repository that already has its own list", async () => {
    const confirm = vi.spyOn(window, "confirm").mockReturnValue(true);
    const fake = fakeApi({
      ...base(view({ followsDefaults: false, applyBase: ["code-review"], checklist: { sections: [], suggested: ["go-style"], budget: 0 } })),
      "PUT /api/repositories/main/acme/payments/settings": { body: repoDetail(repoPayments) },
    });
    renderApp("/repos/main/acme/payments?tab=readiness");
    await userEvent.click(await screen.findByRole("checkbox", { name: "go-style" }));
    await userEvent.click(screen.getByRole("button", { name: "Use these skills" }));
    expect(confirm).not.toHaveBeenCalled();
    const puts = fake.find("PUT", "/api/repositories/main/acme/payments/settings");
    expect(puts).toHaveLength(1);
    expect(puts[0].body).toEqual({ skills: ["code-review", "go-style"] });
    confirm.mockRestore();
  });

  it("clears the ticked skills after a save, so the button waits for a new tick", async () => {
    const fake = fakeApi({
      // The answer after the save still suggests the skill (a stale read).
      ...base(view({ checklist: { sections: [], suggested: ["go-style"], budget: 0 } })),
      "PUT /api/repositories/main/acme/payments/settings": { body: repoDetail(repoPayments) },
    });
    renderApp("/repos/main/acme/payments?tab=readiness");
    const box = await screen.findByRole("checkbox", { name: "go-style" });
    await userEvent.click(box);
    expect(box).toBeChecked();
    await userEvent.click(screen.getByRole("button", { name: "Use these skills" }));
    expect(await screen.findByText("Saved.")).toBeInTheDocument();
    expect(fake.find("PUT", "/api/repositories/main/acme/payments/settings")).toHaveLength(1);
    expect(screen.getByRole("checkbox", { name: "go-style" })).not.toBeChecked();
    expect(screen.getByRole("button", { name: "Use these skills" })).toBeDisabled();
  });

  it("enables Use these skills only for ticked skills that are still suggested", async () => {
    let suggested = ["alpha", "beta"];
    const fake = fakeApi({
      ...base(view()),
      "GET /api/repositories/main/acme/payments/readiness": () => ({
        body: view({ checklist: { sections: [], suggested, budget: 0 } }),
      }),
      "POST /api/repositories/main/acme/payments/readiness/scan": () => {
        // A new scan no longer suggests alpha.
        suggested = ["beta"];
        return { status: 202, body: { scan: scan({ id: "s2", state: "queued", finishedAt: null }) } };
      },
      "PUT /api/repositories/main/acme/payments/settings": { body: repoDetail(repoPayments) },
    });
    renderApp("/repos/main/acme/payments?tab=readiness");
    await userEvent.click(await screen.findByRole("checkbox", { name: "alpha" }));
    expect(screen.getByRole("button", { name: "Use these skills" })).toBeEnabled();
    await userEvent.click(screen.getByRole("button", { name: "Scan now" }));
    await waitForGone("alpha");
    expect(screen.getByRole("button", { name: "Use these skills" })).toBeDisabled();
    await userEvent.click(screen.getByRole("checkbox", { name: "beta" }));
    await userEvent.click(screen.getByRole("button", { name: "Use these skills" }));
    expect(fake.find("PUT", "/api/repositories/main/acme/payments/settings")[0].body).toEqual({ skills: ["code-review", "beta"] });
  });

  it("shows the skills line to paste for a config repository, with no button", async () => {
    fakeApi({
      ...base(
        view({
          configRepo: true,
          applyBase: ["code-review"],
          checklist: { sections: [], suggested: ["security-review", "go-style"], budget: 0 },
        }),
        repoWidgets,
      ),
      "GET /api/reviews?project=acme%2Fwidgets&days=90": { body: { items: [], nextCursor: "" } },
    });
    renderApp("/repos/main/acme/widgets?tab=readiness");
    await userEvent.click(await screen.findByRole("checkbox", { name: "go-style" }));
    expect(screen.queryByRole("button", { name: "Use these skills" })).not.toBeInTheDocument();
    const line = await screen.findByText("skills: [code-review, go-style]");
    expect(line.tagName).toBe("PRE");
  });

  it("shows suggestions without checkboxes to a viewer of a dashboard repository", async () => {
    fakeApi(base(view({ canScan: false, checklist: { sections: [], suggested: ["go-style"], budget: 0 } })));
    renderApp("/repos/main/acme/payments?tab=readiness");
    expect(await screen.findByText("go-style")).toBeInTheDocument();
    expect(screen.queryByRole("checkbox")).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Use these skills" })).not.toBeInTheDocument();
  });

  it("lists at most 50 noise paths and the count of the rest", async () => {
    const noise = Array.from({ length: 60 }, (_, i) => ({ path: `gen/file-${i}.pb.go`, why: "generated", size: 1024 * (100 - i) }));
    fakeApi(
      base(
        view({
          checklist: {
            sections: [{ key: "size", items: [item("noise", { state: "warning", reason: "70 files", noise, more: 10 })] }],
            suggested: [],
            budget: 0,
          },
        }),
      ),
    );
    renderApp("/repos/main/acme/payments?tab=readiness");
    expect(await screen.findByText("gen/file-0.pb.go")).toBeInTheDocument();
    expect(screen.getByText("gen/file-49.pb.go")).toBeInTheDocument();
    expect(screen.queryByText("gen/file-50.pb.go")).not.toBeInTheDocument();
    // 10 the API left out plus the 10 the page cut.
    expect(screen.getByText("and 20 more")).toBeInTheDocument();
  });

  it("places the Readiness tab right after Skills & instructions", async () => {
    fakeApi(base(view()));
    renderApp("/repos/main/acme/payments");
    const tabs = await screen.findByRole("tablist");
    const names = within(tabs)
      .getAllByRole("tab")
      .map((t) => t.textContent);
    expect(names).toEqual(["Reviews", "Settings", "Skills & instructions", "Readiness", "Setup checks (1)"]);
  });

  it("links the 3 readiness Needs attention kinds to the Readiness tab, and the others to Setup checks", async () => {
    const row = (project: string, kind: string, message: string) => ({
      instance: "main",
      project,
      kind,
      message,
      updatedAt: new Date().toISOString(),
    });
    fakeApi({
      "GET /api/me": { body: platformAdmin },
      "GET /api/overview?days=7": {
        body: {
          ...overview,
          attention: [
            row("acme/a", "no_skill_coverage", "No skill matches."),
            row("acme/b", "inputs_over_budget", "Inputs are over 900 KiB."),
            row("acme/c", "no_project_context", "No README."),
            row("acme/d", "single_approval", "One approval."),
          ],
        },
      },
    });
    renderApp("/");
    const region = await screen.findByRole("region", { name: "Needs attention" });
    for (const [project, tab] of [
      ["acme/a", "readiness"],
      ["acme/b", "readiness"],
      ["acme/c", "readiness"],
      ["acme/d", "setup"],
    ]) {
      expect(within(region).getByRole("link", { name: project })).toHaveAttribute("href", `/repos/main/${project}?tab=${tab}`);
    }
    expect(within(region).getByText("No skill matches.")).toBeInTheDocument();
  });
});
