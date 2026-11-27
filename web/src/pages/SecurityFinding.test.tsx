import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import type { SecurityFindingView } from "../api";
import { fakeApi } from "../test/fakeApi";
import {
  repoDetail,
  repoPayments,
  repoToolsPaused,
  reviewBlocking,
  securityFindingDismissed,
  securityFindingOpen,
  securityView,
  ssoAdmin,
} from "../test/fixtures";
import { renderApp } from "../test/render";

afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

const sec = "/api/repositories/main/acme/payments/security";
const one = (id: number) => `${sec}/findings/${id}`;
const page = (id: number) => `/repos/main/acme/payments?tab=security&finding=${id}`;

const routes = (f: SecurityFindingView, role: "admin" | "viewer" = "admin") => ({
  "GET /api/me": { body: ssoAdmin },
  "GET /api/repositories/main/acme/payments": { body: repoDetail(repoPayments, role) },
  "GET /api/reviews?project=acme%2Fpayments&days=90": { body: { items: [reviewBlocking], nextCursor: "" } },
  [`GET ${sec}`]: { body: securityView },
  [`GET ${one(f.finding.id)}`]: { body: f },
});

describe("SecurityFinding", () => {
  it("shows the description, exploit scenario, fix hint and a code link", async () => {
    fakeApi(routes(securityFindingOpen));
    renderApp(page(7));
    expect(await screen.findByRole("heading", { name: "SQL built from a request value" })).toBeInTheDocument();
    expect(screen.getByText("The query joins the search text into the SQL.")).toBeInTheDocument();
    expect(screen.getByText("A search text with a quote reads other rows.")).toBeInTheDocument();
    expect(screen.getByText("Pass the search text as a query argument.")).toBeInTheDocument();
    expect(screen.getByText("High")).toBeInTheDocument();
    expect(screen.getByText("injection")).toBeInTheDocument();
    // The CWE shows beside the rule, and again as the tag it came from.
    expect(screen.getAllByText("CWE-89")).toHaveLength(2);
    expect(screen.getByText("sql")).toBeInTheDocument();
    expect(screen.getByText("Needs validation")).toBeInTheDocument();
    expect(screen.getByText(/Confidence: high/)).toBeInTheDocument();
    const link = screen.getByRole("link", { name: "internal/store/search.go:42" });
    expect(link).toHaveAttribute("href", securityFindingOpen.codeUrl);
  });

  it("shows model text as plain text, never markdown or HTML", async () => {
    const f = { ...securityFindingOpen, description: "**bold** <b>x</b>\nline two", fixHint: "# head" };
    fakeApi(routes(f));
    const { container } = renderApp(page(7));
    expect(await screen.findByText(/\*\*bold\*\* <b>x<\/b>/)).toBeInTheDocument();
    expect(container.querySelector("b, strong")).toBeNull();
    expect(screen.getByText("# head")).toBeInTheDocument();
  });

  it("has no link when codeUrl is empty", async () => {
    fakeApi(routes({ ...securityFindingOpen, codeUrl: "" }));
    renderApp(page(7));
    await screen.findByRole("heading", { name: "SQL built from a request value" });
    expect(screen.getByText("internal/store/search.go:42")).toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "internal/store/search.go:42" })).not.toBeInTheDocument();
  });

  it("does not link a codeUrl that is not http or https", async () => {
    fakeApi(routes({ ...securityFindingOpen, codeUrl: "javascript:alert(1)" }));
    renderApp(page(7));
    await screen.findByRole("heading", { name: "SQL built from a request value" });
    expect(screen.queryByRole("link", { name: "internal/store/search.go:42" })).not.toBeInTheDocument();
  });

  it("shows the history in order", async () => {
    const f: SecurityFindingView = {
      ...securityFindingDismissed,
      history: [
        { kind: "found", scan: 1, actor: "", reason: "", comment: "", at: "2026-11-20T09:20:05Z" },
        { kind: "fixed", scan: 2, actor: "", reason: "", comment: "", at: "2026-11-21T09:20:05Z" },
        { kind: "reopened", scan: 3, actor: "", reason: "", comment: "", at: "2026-11-22T09:20:05Z" },
        { kind: "dismissed", scan: 0, actor: "bob", reason: "used_in_tests", comment: "A test key.", at: "2026-11-23T10:00:00Z" },
        { kind: "restored", scan: 0, actor: "alice", reason: "", comment: "", at: "2026-11-24T10:00:00Z" },
      ],
    };
    fakeApi(routes(f));
    renderApp(page(8));
    const items = await within(await screen.findByRole("list", { name: "History" })).findAllByRole("listitem");
    expect(items.map((i) => i.textContent)).toEqual([
      expect.stringMatching(/^Found in scan 1 · /),
      expect.stringMatching(/^Fixed in scan 2 · /),
      expect.stringMatching(/^Reopened in scan 3 · /),
      expect.stringMatching(/^Dismissed by bob · used in tests · A test key\. · /),
      expect.stringMatching(/^Restored by alice · /),
    ]);
  });

  it("a dismissed finding shows the reason and the comment, and Restore", async () => {
    fakeApi(routes(securityFindingDismissed));
    renderApp(page(8));
    expect(await screen.findByText(/Dismissed by bob: used in tests/)).toBeInTheDocument();
    expect(screen.getAllByText(/A test key\./).length).toBeGreaterThan(0);
    expect(screen.getByRole("button", { name: "Restore" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Dismiss" })).not.toBeInTheDocument();
  });

  it("Dismiss needs a reason and posts the reason and the comment", async () => {
    const user = userEvent.setup();
    const after: SecurityFindingView = { ...securityFindingDismissed, finding: { ...securityFindingDismissed.finding, id: 7 } };
    const fake = fakeApi({ ...routes(securityFindingOpen), [`POST ${one(7)}/dismiss`]: { body: after } });
    renderApp(page(7));
    await user.click(await screen.findByRole("button", { name: "Dismiss" }));
    const box = await screen.findByRole("dialog");
    const confirm = within(box).getByRole("button", { name: "Dismiss finding" });
    expect(confirm).toBeDisabled();
    expect(within(box).getAllByRole("radio").map((r) => (r as HTMLInputElement).value)).toEqual([
      "false_positive",
      "wont_fix",
      "used_in_tests",
      "mitigated",
      "acceptable_risk",
    ]);
    for (const t of ["false positive", "won't fix", "used in tests", "mitigated", "acceptable risk"]) {
      expect(within(box).getByRole("radio", { name: t })).toBeInTheDocument();
    }
    expect(within(box).getByRole("textbox", { name: "Comment (optional)" })).toHaveAttribute("maxlength", "1000");
    await user.click(within(box).getByRole("radio", { name: "used in tests" }));
    await user.type(within(box).getByRole("textbox", { name: "Comment (optional)" }), "A test key.");
    expect(confirm).toBeEnabled();
    await user.click(confirm);
    await vi.waitFor(() => expect(fake.find("POST", `${one(7)}/dismiss`)).toHaveLength(1));
    expect(fake.find("POST", `${one(7)}/dismiss`)[0].body).toEqual({ reason: "used_in_tests", comment: "A test key." });
    // The page shows the answer: dismissed, with its reason.
    expect(await screen.findByText(/Dismissed by bob: used in tests/)).toBeInTheDocument();
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  });

  it("shows the server's refusal as it is and keeps the box open", async () => {
    const user = userEvent.setup();
    fakeApi({
      ...routes(securityFindingOpen),
      [`POST ${one(7)}/dismiss`]: { status: 409, body: { error: "a fixed finding can't be dismissed" } },
    });
    renderApp(page(7));
    await user.click(await screen.findByRole("button", { name: "Dismiss" }));
    const box = await screen.findByRole("dialog");
    await user.click(within(box).getByRole("radio", { name: "mitigated" }));
    await user.click(within(box).getByRole("button", { name: "Dismiss finding" }));
    expect(await within(box).findByText("a fixed finding can't be dismissed")).toBeInTheDocument();
  });

  it("Restore posts and shows the finding open", async () => {
    const user = userEvent.setup();
    const back: SecurityFindingView = { ...securityFindingOpen, finding: { ...securityFindingOpen.finding, id: 8 } };
    const fake = fakeApi({ ...routes(securityFindingDismissed), [`POST ${one(8)}/restore`]: { body: back } });
    renderApp(page(8));
    await user.click(await screen.findByRole("button", { name: "Restore" }));
    await vi.waitFor(() => expect(fake.find("POST", `${one(8)}/restore`)).toHaveLength(1));
    expect(fake.find("POST", `${one(8)}/restore`)[0].body).toEqual({});
    expect(await screen.findByRole("button", { name: "Dismiss" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Restore" })).not.toBeInTheDocument();
  });

  it("a viewer gets neither button", async () => {
    fakeApi(routes({ ...securityFindingOpen, canManage: false }, "viewer"));
    renderApp(page(7));
    await screen.findByRole("heading", { name: "SQL built from a request value" });
    expect(screen.queryByRole("button", { name: "Dismiss" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Restore" })).not.toBeInTheDocument();
  });

  it("a paused repository shows neither button, and no error", async () => {
    fakeApi({
      "GET /api/me": { body: ssoAdmin },
      "GET /api/repositories/main/acme/tools": { body: repoDetail(repoToolsPaused, "admin") },
      "GET /api/repositories/main/acme/tools/security": { body: securityView },
      "GET /api/repositories/main/acme/tools/security/findings/7": { body: securityFindingOpen },
    });
    renderApp("/repos/main/acme/tools?tab=security&finding=7");
    await screen.findByRole("heading", { name: "SQL built from a request value" });
    expect(screen.queryByRole("button", { name: "Dismiss" })).not.toBeInTheDocument();
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  });

  it("a fixed finding has neither button", async () => {
    const f = { ...securityFindingOpen, finding: { ...securityFindingOpen.finding, state: "fixed" as const } };
    fakeApi(routes(f));
    renderApp(page(7));
    await screen.findByRole("heading", { name: "SQL built from a request value" });
    expect(screen.queryByRole("button", { name: "Dismiss" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Restore" })).not.toBeInTheDocument();
  });

  it("a back link returns to the findings", async () => {
    fakeApi(routes(securityFindingOpen));
    renderApp(page(7));
    expect(await screen.findByRole("link", { name: "Back to findings" })).toHaveAttribute("href", "/repos/main/acme/payments?tab=security");
  });
});
