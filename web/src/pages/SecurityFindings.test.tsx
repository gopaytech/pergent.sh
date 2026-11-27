import { act, fireEvent, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import type { SecurityFindingRowView, SecurityFindingsView, SecurityView } from "../api";
import { fakeApi } from "../test/fakeApi";
import { repoDetail, repoPayments, reviewBlocking, securityFindingDismissed, securityFindingOpen, securityView, ssoAdmin } from "../test/fixtures";
import { renderApp } from "../test/render";

afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

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

const injection: SecurityFindingRowView = securityFindingOpen.finding;
const secrets: SecurityFindingRowView = { ...securityFindingDismissed.finding, id: 9, state: "open", severity: "low", title: "A key in a test file" };
const list = (findings: SecurityFindingRowView[], truncated = false): SecurityFindingsView => ({ findings, truncated });

const routes = (extra: Record<string, unknown> = {}, view: SecurityView = securityView) => ({
  "GET /api/me": { body: ssoAdmin },
  "GET /api/repositories/main/acme/payments": { body: repoDetail(repoPayments, "admin") },
  "GET /api/reviews?project=acme%2Fpayments&days=90": { body: { items: [reviewBlocking], nextCursor: "" } },
  [`GET ${sec}`]: { body: view },
  ...Object.fromEntries(Object.entries(extra).map(([k, v]) => [k, { body: v }])),
});

describe("SecurityFindings", () => {
  it("asks for the open findings first by default", async () => {
    const fake = fakeApi(routes({ [`GET ${sec}/findings?state=open`]: list([injection, secrets]) }));
    renderApp(open);
    expect(await screen.findByRole("link", { name: /SQL built from a request value/ })).toHaveAttribute(
      "href",
      "/repos/main/acme/payments?tab=security&finding=7",
    );
    expect(screen.getByLabelText("State")).toHaveValue("open");
    expect(fake.find("GET", `${sec}/findings?state=open`)).toHaveLength(1);
    const row = screen.getByRole("link", { name: /SQL built from a request value/ }).closest("tr")!;
    expect(row).toHaveTextContent("High");
    expect(row).toHaveTextContent("internal/store/search.go:42");
  });

  it("the severity and Rule filters change the query and stay in the address", async () => {
    const user = userEvent.setup();
    const fake = fakeApi(
      routes({
        [`GET ${sec}/findings?state=open`]: list([injection, secrets]),
        [`GET ${sec}/findings?state=open&severity=high`]: list([injection]),
        [`GET ${sec}/findings?state=open&severity=high&rule=injection`]: list([injection]),
        [`GET ${sec}/findings?state=dismissed&severity=high&rule=injection`]: list([]),
      }),
    );
    renderApp(open);
    await screen.findByRole("link", { name: /SQL built from/ });
    // The Rule select is built from the rules in the answer, and is not called CWE.
    const rule = screen.getByLabelText("Rule");
    expect(within(rule).getAllByRole("option").map((o) => o.textContent)).toEqual(["All rules", "injection", "secrets"]);
    expect(screen.queryByLabelText("CWE")).not.toBeInTheDocument();
    await user.selectOptions(screen.getByLabelText("Severity"), "high");
    await vi.waitFor(() => expect(fake.find("GET", `${sec}/findings?state=open&severity=high`)).toHaveLength(1));
    expect(screen.getByTestId("location")).toHaveTextContent("tab=security&severity=high");
    await user.selectOptions(screen.getByLabelText("Rule"), "injection");
    await vi.waitFor(() => expect(fake.find("GET", `${sec}/findings?state=open&severity=high&rule=injection`)).toHaveLength(1));
    expect(screen.getByTestId("location")).toHaveTextContent("rule=injection");
    // The rule options stay while a rule is chosen.
    expect(within(screen.getByLabelText("Rule")).getAllByRole("option").map((o) => o.textContent)).toEqual(["All rules", "injection", "secrets"]);
    await user.selectOptions(screen.getByLabelText("State"), "dismissed");
    await vi.waitFor(() => expect(fake.find("GET", `${sec}/findings?state=dismissed&severity=high&rule=injection`)).toHaveLength(1));
  });

  it("the search sends q after typing stops and stays in the address", async () => {
    vi.useFakeTimers({ toFake: ["setTimeout", "clearTimeout", "setInterval", "clearInterval", "Date"] });
        const fake = fakeApi(
      routes({ [`GET ${sec}/findings?state=open`]: list([injection, secrets]), [`GET ${sec}/findings?state=open&q=search.go`]: list([injection]) }),
    );
    renderApp(open);
    await settle();
    fireEvent.change(screen.getByRole("searchbox", { name: "Search findings" }), { target: { value: "search.go" } });
    // Nothing is sent while the typing goes on.
    expect(fake.find("GET", `${sec}/findings?state=open&q=search.go`)).toHaveLength(0);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(400);
    });
    await settle();
    expect(fake.find("GET", `${sec}/findings?state=open&q=search.go`)).toHaveLength(1);
    expect(screen.getByTestId("location")).toHaveTextContent("q=search.go");
  });

  it("the Rule column shows a CWE only when the finding has one", async () => {
    fakeApi(routes({ [`GET ${sec}/findings?state=open`]: list([injection, secrets]) }));
    renderApp(open);
    const a = (await screen.findByRole("link", { name: /SQL built from/ })).closest("tr")!;
    expect(a).toHaveTextContent("injection");
    expect(a).toHaveTextContent("CWE-89");
    const b = screen.getByRole("link", { name: secrets.title }).closest("tr")!;
    expect(b).toHaveTextContent("secrets");
    expect(b).not.toHaveTextContent("CWE");
  });

  it("shows the Needs validation badge on a finding that needs it", async () => {
    fakeApi(routes({ [`GET ${sec}/findings?state=open`]: list([injection, secrets]) }));
    renderApp(open);
    const a = (await screen.findByRole("link", { name: /SQL built from/ })).closest("tr")!;
    expect(within(a).getByText("Needs validation")).toBeInTheDocument();
    expect(within(screen.getByRole("link", { name: secrets.title }).closest("tr")!).queryByText("Needs validation")).not.toBeInTheDocument();
  });

  it("says when the list was cut at 1000", async () => {
    fakeApi(routes({ [`GET ${sec}/findings?state=open`]: list([injection], true) }));
    renderApp(open);
    expect(await screen.findByText("Showing the first 1000. Narrow the filters to see more.")).toBeInTheDocument();
  });

  it("shows a title as text, never as HTML", async () => {
    const odd = { ...injection, title: "<img src=x onerror=alert(1)>" };
    fakeApi(routes({ [`GET ${sec}/findings?state=open`]: list([odd]) }));
    const { container } = renderApp(open);
    expect(await screen.findByText("<img src=x onerror=alert(1)>")).toBeInTheDocument();
    expect(container.querySelector("img")).toBeNull();
  });

  it("says so when no finding matches", async () => {
    fakeApi(routes({ [`GET ${sec}/findings?state=open`]: list([]) }));
    renderApp(open);
    expect(await screen.findByText("No findings match.")).toBeInTheDocument();
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  });
});
