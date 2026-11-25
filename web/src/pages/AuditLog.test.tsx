import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { fakeApi } from "../test/fakeApi";
import { auditPage1, auditPage2, operator, viewer } from "../test/fixtures";
import { renderApp } from "../test/render";

afterEach(() => vi.unstubAllGlobals());

// entryRows are the table's entries; dayRows the day each group starts with.
const entryRows = () =>
  screen
    .getAllByRole("row")
    .slice(1)
    .filter((r) => !r.classList.contains("day-row"));
const dayRows = () =>
  screen
    .getAllByRole("row")
    .filter((r) => r.classList.contains("day-row"))
    .map((r) => r.textContent);

describe("AuditLog", () => {
  it("lists the entries, newest first, a day at a time, with who, what and where", async () => {
    fakeApi({ "GET /api/me": { body: operator }, "GET /api/audit": { body: auditPage1 } });
    renderApp("/admin/audit");
    expect(await screen.findByRole("heading", { name: "Audit log" })).toBeInTheDocument();
    await screen.findByText("Team created");
    expect(dayRows()).toEqual(["Fri 30 Oct 2026"]);
    const rows = entryRows();
    expect(rows).toHaveLength(2);
    expect(within(rows[0]).getByText("14:02")).toHaveAttribute("title", "2026-10-30 14:02 UTC");
    // The actor's name, and where it is from below it.
    expect(rows[0]).toHaveTextContent("admin");
    expect(within(rows[0]).getByText("password")).toHaveClass("meta");
    expect(within(rows[0]).getByText("Team created")).toHaveClass("pill", "grey");
    expect(rows[0]).toHaveTextContent("Payments (maintainer)");
    expect(rows[1]).toHaveTextContent("Repository on");
    // The repository links to its page.
    expect(within(rows[1]).getByRole("link", { name: /acme\/payments/ })).toHaveAttribute("href", "/repos/main/acme/payments");
  });

  it("shows a command's detail pair by pair: a short SHA, the change linked, and the action coloured", async () => {
    const entry = {
      id: 40,
      at: "2026-11-25T04:04:00Z",
      actor: "ana (gitlab main)",
      action: "command_refused",
      instance: "main",
      project: "acme/payments",
      detail: "mr=1895 head=58dcf4b5b07aa75c1b6469aa command=dismiss",
    };
    fakeApi({ "GET /api/me": { body: operator }, "GET /api/audit": { body: { entries: [entry], next: 0 } } });
    renderApp("/admin/audit");
    const row = (await screen.findByText("Command refused")).closest("tr") as HTMLElement;
    expect(within(row).getByText("Command refused")).toHaveClass("pill", "warn");
    expect(within(row).getByText("gitlab main")).toHaveClass("meta");
    expect(within(row).getByRole("link", { name: "1895" })).toHaveAttribute("href", "/reviews/main/acme/payments/1895");
    expect(within(row).getByText("58dcf4b5")).toHaveAttribute("title", "58dcf4b5b07aa75c1b6469aa");
    expect(within(row).getByText("dismiss")).toBeInTheDocument();
  });

  it("searches the whole log on the server, keeps the search in the address, and says when nothing matches", async () => {
    const fake = fakeApi({
      "GET /api/me": { body: operator },
      "GET /api/audit": { body: auditPage1 },
      "GET /api/audit?q=payments": { body: { entries: [auditPage1.entries[1]], next: 0 } },
      "GET /api/audit?q=nobody": { body: { entries: [], next: 0 } },
    });
    renderApp("/admin/audit");
    await screen.findByText("Team created");
    await userEvent.type(screen.getByRole("searchbox", { name: "Search the audit log" }), "payments");
    await waitFor(() => expect(screen.getByTestId("location")).toHaveTextContent("/admin/audit?q=payments"));
    await waitFor(() => expect(screen.queryByText("Team created")).not.toBeInTheDocument());
    expect(await screen.findByText("Repository on")).toBeInTheDocument();
    expect(entryRows()).toHaveLength(1);
    // Typing waits for a pause: one request for the whole word, none for its first letters.
    expect(fake.find("GET", "/api/audit?q=payments")).toHaveLength(1);
    expect(fake.find("GET", "/api/audit?q=p")).toHaveLength(0);
    await userEvent.clear(screen.getByRole("searchbox", { name: "Search the audit log" }));
    await userEvent.type(screen.getByRole("searchbox", { name: "Search the audit log" }), "nobody");
    expect(await screen.findByText('No entry holds "nobody". Try a shorter search, or clear it.')).toBeInTheDocument();
  });

  it("opens with the search in the address", async () => {
    const fake = fakeApi({
      "GET /api/me": { body: operator },
      "GET /api/audit?q=payments": { body: { entries: [auditPage1.entries[1]], next: 0 } },
    });
    renderApp("/admin/audit?q=payments");
    expect(await screen.findByText("Repository on")).toBeInTheDocument();
    expect(screen.getByRole("searchbox", { name: "Search the audit log" })).toHaveValue("payments");
    expect(fake.find("GET", "/api/audit?q=payments")).toHaveLength(1);
    expect(fake.find("GET", "/api/audit")).toHaveLength(0);
  });

  it("pages back with before, until the log ends", async () => {
    const fake = fakeApi({
      "GET /api/me": { body: operator },
      "GET /api/audit": { body: auditPage1 },
      "GET /api/audit?before=29": { body: auditPage2 },
    });
    renderApp("/admin/audit");
    await userEvent.click(await screen.findByRole("button", { name: "Load more" }));
    expect(await screen.findByText("old-user")).toBeInTheDocument();
    expect(fake.find("GET", "/api/audit?before=29")).toHaveLength(1);
    // Both pages are in the table, a day row for each day, and nothing is older.
    expect(entryRows()).toHaveLength(3);
    expect(dayRows()).toEqual(["Fri 30 Oct 2026", "Thu 29 Oct 2026"]);
    expect(screen.queryByRole("button", { name: "Load more" })).not.toBeInTheDocument();
  });

  it("keeps the entries and Load more when it fails, with one error", async () => {
    let tries = 0;
    const fake = fakeApi({
      "GET /api/me": { body: operator },
      "GET /api/audit": { body: auditPage1 },
      "GET /api/audit?before=29": () =>
        tries++ === 0 ? { status: 500, body: { error: "could not read the log" } } : { body: auditPage2 },
    });
    renderApp("/admin/audit");
    await userEvent.click(await screen.findByRole("button", { name: "Load more" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("could not read the log");
    expect(screen.getAllByRole("alert")).toHaveLength(1);
    expect(entryRows()).toHaveLength(2);
    // Retrying works.
    await userEvent.click(screen.getByRole("button", { name: "Load more" }));
    expect(await screen.findByText("old-user")).toBeInTheDocument();
    expect(fake.find("GET", "/api/audit?before=29")).toHaveLength(2);
    await vi.waitFor(() => expect(screen.queryByRole("alert")).not.toBeInTheDocument());
  });

  it("has no Older button when the whole log fits one page", async () => {
    fakeApi({ "GET /api/me": { body: operator }, "GET /api/audit": { body: auditPage2 } });
    renderApp("/admin/audit");
    await screen.findByText("old-user");
    expect(screen.queryByRole("button", { name: "Load more" })).not.toBeInTheDocument();
  });

  it("says so when nothing is recorded", async () => {
    fakeApi({ "GET /api/me": { body: operator }, "GET /api/audit": { body: { entries: [], next: 0 } } });
    renderApp("/admin/audit");
    expect(await screen.findByText("Nothing is recorded yet.")).toBeInTheDocument();
  });

  it("shows the text of the entries as text, never as HTML", async () => {
    fakeApi({
      "GET /api/me": { body: operator },
      "GET /api/audit": {
        body: { entries: [{ ...auditPage2.entries[0], detail: "<img src=x onerror=alert(1)>" }], next: 0 },
      },
    });
    renderApp("/admin/audit");
    const row = (await screen.findByText(/<img src=x/)).closest("tr") as HTMLElement;
    expect(within(row).queryByRole("img")).not.toBeInTheDocument();
  });

  it("isn't shown to a viewer", async () => {
    const fake = fakeApi({ "GET /api/me": { body: viewer } });
    renderApp("/admin/audit");
    expect(await screen.findByRole("heading", { name: "Not found" })).toBeInTheDocument();
    expect(fake.find("GET", "/api/audit")).toHaveLength(0);
  });
});
