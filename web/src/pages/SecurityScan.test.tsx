import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import type { SecurityScanDetailView, SecurityScanView } from "../api";
import { fakeApi } from "../test/fakeApi";
import {
  repoDetail,
  repoPayments,
  reviewBlocking,
  securityFindingDismissed,
  securityFindingOpen,
  securityScanComplete,
  securityScanPartial,
  securityView,
  ssoAdmin,
  tok,
} from "../test/fixtures";
import { renderApp } from "../test/render";

afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

const sec = "/api/repositories/main/acme/payments/security";
const page = (id: string, extra = "") => `/repos/main/acme/payments?tab=security${extra}&scan=${id}`;

const detail = (scan: SecurityScanView, extra: Partial<SecurityScanDetailView> = {}): SecurityScanDetailView => ({
  scan,
  log: "scan started\nread 12 files",
  new: [securityFindingOpen.finding],
  fixed: [{ ...securityFindingDismissed.finding, title: "A key in a test file" }],
  ...extra,
});

const routes = (d: SecurityScanDetailView) => ({
  "GET /api/me": { body: ssoAdmin },
  "GET /api/repositories/main/acme/payments": { body: repoDetail(repoPayments, "admin") },
  "GET /api/reviews?project=acme%2Fpayments&days=90": { body: { items: [reviewBlocking], nextCursor: "" } },
  [`GET ${sec}`]: { body: securityView },
  [`GET ${sec}/findings?state=open`]: { body: { findings: [], truncated: false } },
  [`GET ${sec}/scans`]: { body: { scans: [d.scan] } },
  [`GET ${sec}/scans/${d.scan.id}`]: { body: d },
});

const id = securityScanComplete.id;

describe("SecurityScan", () => {
  it("shows coverage, usage and the log", async () => {
    fakeApi(routes(detail({ ...securityScanComplete, tokens: tok(1_000, 4_000) })));
    renderApp(page(id));
    expect(await screen.findByRole("heading", { name: "Scan 1" })).toBeInTheDocument();
    expect(screen.getByText("Complete")).toBeInTheDocument();
    expect(screen.getByText("Coverage: 12 of 12 planned")).toBeInTheDocument();
    expect(within(screen.getByRole("table", { name: "Tokens" })).getByText("Prompt")).toBeInTheDocument();
    expect(screen.getByText(/scan started/)).toHaveTextContent("read 12 files");
  });

  it("says when no coverage was reported", async () => {
    fakeApi(routes(detail({ ...securityScanComplete, coverage: null })));
    renderApp(page(id));
    expect(await screen.findByText("Coverage not reported")).toBeInTheDocument();
  });

  it("shows the reason as the server wrote it", async () => {
    fakeApi(routes(detail({ ...securityScanPartial, reason: "cancelled" })));
    renderApp(page(securityScanPartial.id));
    expect(await screen.findByText("Reason: cancelled")).toBeInTheDocument();
  });

  it("a stream usage source shows the sub-agent note", async () => {
    fakeApi(routes(detail({ ...securityScanComplete, usageSource: "stream" })));
    renderApp(page(id));
    expect(await screen.findByText("Sub-agent tokens are missing: only the main agent's tokens were counted.")).toBeInTheDocument();
  });

  it("no sub-agent note for stats", async () => {
    fakeApi(routes(detail(securityScanComplete)));
    renderApp(page(id));
    await screen.findByRole("heading", { name: "Scan 1" });
    expect(screen.queryByText(/Sub-agent tokens/)).not.toBeInTheDocument();
  });

  it("the Git LFS and submodules line", async () => {
    fakeApi(routes(detail(securityScanComplete)));
    renderApp(page(id));
    expect(
      await screen.findByText("The scan read the platform's archive of this commit: Git LFS files and submodules are not in it."),
    ).toBeInTheDocument();
  });

  it("shows the skill line when the skill changed", async () => {
    fakeApi(routes(detail({ ...securityScanComplete, skillUpdated: true })));
    renderApp(page(id));
    expect(await screen.findByText("Skill updated: some findings may show as new")).toBeInTheDocument();
  });

  it("lists new and fixed findings with links", async () => {
    fakeApi(routes(detail(securityScanComplete)));
    renderApp(page(id, "&state=fixed"));
    const added = await screen.findByRole("region", { name: "New findings" });
    // A finding link drops scan and keeps the list filters.
    expect(within(added).getByRole("link", { name: /SQL built from a request value/ })).toHaveAttribute(
      "href",
      "/repos/main/acme/payments?tab=security&state=fixed&finding=7",
    );
    const fixed = screen.getByRole("region", { name: "Fixed findings" });
    expect(within(fixed).getByRole("link", { name: /A key in a test file/ })).toHaveAttribute("href", expect.stringContaining("finding=8"));
    expect(within(fixed).getByRole("link", { name: /A key in a test file/ }).getAttribute("href")).not.toContain("scan=");
  });

  it("Download SARIF links to the file", async () => {
    fakeApi(routes(detail(securityScanComplete)));
    renderApp(page(id));
    const link = await screen.findByRole("link", { name: "Download SARIF" });
    expect(link).toHaveAttribute("href", `${sec}/scans/${id}/sarif`);
    expect(link).toHaveAttribute("download");
  });

  it("no download once dropped", async () => {
    fakeApi(routes(detail({ ...securityScanComplete, hasSarif: false })));
    renderApp(page(id));
    await screen.findByRole("heading", { name: "Scan 1" });
    expect(screen.queryByRole("link", { name: "Download SARIF" })).not.toBeInTheDocument();
  });

  it("shows the log as plain text", async () => {
    fakeApi(routes(detail(securityScanComplete, { log: "**x** <b>y</b>" })));
    const { container } = renderApp(page(id));
    expect(await screen.findByText("**x** <b>y</b>")).toBeInTheDocument();
    expect(container.querySelector("pre b, pre strong")).toBeNull();
  });

  it("Back keeps the findings filters and drops the scan", async () => {
    fakeApi(routes(detail(securityScanComplete)));
    renderApp(page(id, "&state=fixed&severity=high&rule=injection&q=sql"));
    const back = await screen.findByRole("link", { name: "Back to findings" });
    const url = new URL(back.getAttribute("href") ?? "", "http://x");
    expect(url.searchParams.get("scan")).toBeNull();
    expect(url.searchParams.get("finding")).toBeNull();
    expect(url.searchParams.get("tab")).toBe("security");
    for (const [k, v] of [["state", "fixed"], ["severity", "high"], ["rule", "injection"], ["q", "sql"]]) expect(url.searchParams.get(k)).toBe(v);
  });

  it("a finding in the address wins over a scan", async () => {
    fakeApi({
      ...routes(detail(securityScanComplete)),
      [`GET ${sec}/findings/7`]: { body: securityFindingOpen },
    });
    renderApp(`/repos/main/acme/payments?tab=security&scan=${id}&finding=7`);
    expect(await screen.findByRole("heading", { name: "SQL built from a request value" })).toBeInTheDocument();
    expect(screen.queryByRole("heading", { name: "Scan 1" })).not.toBeInTheDocument();
    // Back from the finding goes to the list, not to the scan.
    const back = screen.getByRole("link", { name: "Back to findings" });
    expect(back.getAttribute("href")).not.toContain("scan=");
  });

  it("opens a scan's page from a row of the list", async () => {
    fakeApi(routes(detail(securityScanComplete)));
    renderApp("/repos/main/acme/payments?tab=security");
    const user = userEvent.setup();
    await user.click(await screen.findByRole("link", { name: "1" }));
    expect(await screen.findByRole("heading", { name: "Scan 1" })).toBeInTheDocument();
  });
});
