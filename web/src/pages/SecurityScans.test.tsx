import { act, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { fakeApi } from "../test/fakeApi";
import {
  repoDetail,
  repoPayments,
  reviewBlocking,
  securityScanComplete,
  securityScanFailed,
  securityScanRunning,
  securityScanPartial,
  securityView,
  ssoAdmin,
} from "../test/fixtures";
import { renderApp } from "../test/render";

afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

const sec = "/api/repositories/main/acme/payments/security";

const routes = (scans: unknown[]) => ({
  "GET /api/me": { body: ssoAdmin },
  "GET /api/repositories/main/acme/payments": { body: repoDetail(repoPayments, "admin") },
  "GET /api/reviews?project=acme%2Fpayments&days=90": { body: { items: [reviewBlocking], nextCursor: "" } },
  [`GET ${sec}`]: { body: securityView },
  [`GET ${sec}/findings?state=open`]: { body: { findings: [], truncated: false } },
  [`GET ${sec}/scans`]: { body: { scans } },
});

describe("SecurityScans", () => {
  it("lists scans newest first with new and fixed counts", async () => {
    // The API sends the newest first; the list keeps its order.
    fakeApi(routes([securityScanPartial, securityScanComplete]));
    renderApp("/repos/main/acme/payments?tab=security&state=dismissed");
    const card = await screen.findByRole("region", { name: "Scans" });
    const rows = (await within(card).findAllByRole("row")).slice(1);
    expect(rows).toHaveLength(2);
    expect(rows[0]).toHaveTextContent("2");
    expect(rows[1]).toHaveTextContent("1");
    expect(rows[1]).toHaveTextContent("Complete");
    expect(rows[1]).toHaveTextContent("20m 0s");
    expect(rows[1]).toHaveTextContent("3");
    // A row opens the scan page; the findings filters stay and no finding is carried.
    const link = within(rows[1]).getByRole("link", { name: "1" });
    expect(link).toHaveAttribute("href", `/repos/main/acme/payments?tab=security&state=dismissed&scan=${securityScanComplete.id}`);
  });

  it("a partial and a failed row show their reason", async () => {
    fakeApi(routes([securityScanFailed, securityScanPartial]));
    renderApp("/repos/main/acme/payments?tab=security");
    const card = await screen.findByRole("region", { name: "Scans" });
    expect(await within(card).findByText("the Job could not start")).toBeInTheDocument();
    expect(within(card).getByText("timeout")).toBeInTheDocument();
    expect(within(card).getByText("Failed")).toBeInTheDocument();
    expect(within(card).getByText("Partial")).toBeInTheDocument();
  });

  it("shows the empty line", async () => {
    fakeApi(routes([]));
    renderApp("/repos/main/acme/payments?tab=security");
    expect(await screen.findByText("No scans yet.")).toBeInTheDocument();
  });

  it("shows a scan at once after Start scan", async () => {
    const user = userEvent.setup();
    let started = false;
    const fake = fakeApi({
      ...routes([]),
      [`GET ${sec}/scans`]: () => ({ body: { scans: started ? [securityScanRunning] : [] } }),
      [`GET ${sec}`]: () => ({ body: started ? { ...securityView, canStart: false, running: securityScanRunning } : securityView }),
      [`POST ${sec}/scans`]: () => {
        started = true;
        return { status: 202, body: { scan: securityScanRunning } };
      },
    });
    renderApp("/repos/main/acme/payments?tab=security");
    await user.click(await screen.findByRole("button", { name: "Start scan" }));
    const box = await screen.findByRole("dialog");
    await user.click(within(box).getByRole("button", { name: "Start scan" }));
    const card = screen.getByRole("region", { name: "Scans" });
    expect(await within(card).findByText("Running")).toBeInTheDocument();
    expect(fake.find("GET", `${sec}/scans`)).toHaveLength(2);
  });

  it("refreshes the scans, the findings and the counts when a running scan ends", async () => {
    vi.useFakeTimers({ toFake: ["setTimeout", "clearTimeout", "setInterval", "clearInterval", "Date"] });
    let ended = false;
    const settle = async () => {
      for (let i = 0; i < 20; i++) {
        await act(async () => {
          await new Promise<void>((r) => setImmediate(r));
          await vi.advanceTimersByTimeAsync(1);
        });
      }
    };
    const fake = fakeApi({
      ...routes([]),
      [`GET ${sec}`]: () => ({ body: ended ? securityView : { ...securityView, canStart: false, running: securityScanRunning } }),
      [`GET ${sec}/scans`]: () => ({ body: { scans: [ended ? securityScanComplete : securityScanRunning] } }),
    });
    const { unmount } = renderApp("/repos/main/acme/payments?tab=security");
    await settle();
    const card = screen.getByRole("region", { name: "Scans" });
    expect(within(card).getByText("Running")).toBeInTheDocument();
    expect(fake.find("GET", `${sec}/findings?state=open`)).toHaveLength(1);
    ended = true;
    await vi.advanceTimersByTimeAsync(10_000);
    await settle();
    expect(within(card).getByText("Complete")).toBeInTheDocument();
    expect(fake.find("GET", `${sec}/scans`)).toHaveLength(2);
    expect(fake.find("GET", `${sec}/findings?state=open`)).toHaveLength(2);
    unmount();
  });
});
