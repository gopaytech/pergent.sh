import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { fakeApi } from "../test/fakeApi";
import { auditPage1, auditPage2, operator, viewer } from "../test/fixtures";
import { renderApp } from "../test/render";

afterEach(() => vi.unstubAllGlobals());

describe("AuditLog", () => {
  it("lists the entries, newest first, with who, what and where", async () => {
    fakeApi({ "GET /api/me": { body: operator }, "GET /api/audit": { body: auditPage1 } });
    renderApp("/audit");
    expect(await screen.findByRole("heading", { name: "Audit log" })).toBeInTheDocument();
    const rows = (await screen.findAllByRole("row")).slice(1);
    expect(rows).toHaveLength(2);
    expect(rows[0]).toHaveTextContent("2026-10-30 14:02 UTC");
    expect(rows[0]).toHaveTextContent("admin");
    expect(rows[0]).toHaveTextContent("Team created");
    expect(rows[0]).toHaveTextContent("Payments (maintainer)");
    expect(rows[1]).toHaveTextContent("Repository on");
    expect(rows[1]).toHaveTextContent("main/acme/payments");
  });

  it("pages back with before, until the log ends", async () => {
    const fake = fakeApi({
      "GET /api/me": { body: operator },
      "GET /api/audit": { body: auditPage1 },
      "GET /api/audit?before=29": { body: auditPage2 },
    });
    renderApp("/audit");
    await userEvent.click(await screen.findByRole("button", { name: "Older" }));
    expect(await screen.findByText("old-user")).toBeInTheDocument();
    expect(fake.find("GET", "/api/audit?before=29")).toHaveLength(1);
    // Both pages are in the table, and nothing is older.
    expect(screen.getAllByRole("row")).toHaveLength(1 + 3);
    expect(screen.queryByRole("button", { name: "Older" })).not.toBeInTheDocument();
  });

  it("keeps the entries and the Older button when Older fails, with one error", async () => {
    let tries = 0;
    const fake = fakeApi({
      "GET /api/me": { body: operator },
      "GET /api/audit": { body: auditPage1 },
      "GET /api/audit?before=29": () =>
        tries++ === 0 ? { status: 500, body: { error: "could not read the log" } } : { body: auditPage2 },
    });
    renderApp("/audit");
    await userEvent.click(await screen.findByRole("button", { name: "Older" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("could not read the log");
    expect(screen.getAllByRole("alert")).toHaveLength(1);
    expect(screen.getAllByRole("row")).toHaveLength(1 + 2);
    // Retrying works.
    await userEvent.click(screen.getByRole("button", { name: "Older" }));
    expect(await screen.findByText("old-user")).toBeInTheDocument();
    expect(fake.find("GET", "/api/audit?before=29")).toHaveLength(2);
    await vi.waitFor(() => expect(screen.queryByRole("alert")).not.toBeInTheDocument());
  });

  it("has no Older button when the whole log fits one page", async () => {
    fakeApi({ "GET /api/me": { body: operator }, "GET /api/audit": { body: auditPage2 } });
    renderApp("/audit");
    await screen.findByText("old-user");
    expect(screen.queryByRole("button", { name: "Older" })).not.toBeInTheDocument();
  });

  it("says so when nothing is recorded", async () => {
    fakeApi({ "GET /api/me": { body: operator }, "GET /api/audit": { body: { entries: [], next: 0 } } });
    renderApp("/audit");
    expect(await screen.findByText("Nothing is recorded yet.")).toBeInTheDocument();
  });

  it("shows the text of the entries as text, never as HTML", async () => {
    fakeApi({
      "GET /api/me": { body: operator },
      "GET /api/audit": {
        body: { entries: [{ ...auditPage2.entries[0], detail: "<img src=x onerror=alert(1)>" }], next: 0 },
      },
    });
    renderApp("/audit");
    const row = (await screen.findByText(/<img src=x/)).closest("tr") as HTMLElement;
    expect(within(row).queryByRole("img")).not.toBeInTheDocument();
  });

  it("isn't shown to a viewer", async () => {
    const fake = fakeApi({ "GET /api/me": { body: viewer } });
    renderApp("/audit");
    expect(await screen.findByRole("heading", { name: "Not found" })).toBeInTheDocument();
    expect(fake.find("GET", "/api/audit")).toHaveLength(0);
  });
});
