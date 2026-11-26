import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { fakeApi } from "../test/fakeApi";
import {
  instanceDashboardGitLab,
  instanceGitLab,
  operator,
  repoPayments,
  repoWidgets,
  teamEmpty,
  teamPagePayments,
  teamPayments,
  userDev,
  userOperator,
  userWaiting,
} from "../test/fixtures";
import { renderApp } from "../test/render";

afterEach(() => vi.unstubAllGlobals());

const base = {
  "GET /api/me": { body: operator },
  "GET /api/users": { body: [userWaiting, userDev, userOperator] },
  "GET /api/repositories": { body: [repoPayments, repoWidgets] },
  "GET /api/admin/instances": { body: [instanceGitLab, instanceDashboardGitLab] },
};

describe("Teams", () => {
  it("lists the teams in a table, no Your role column, the name linked to the team page", async () => {
    fakeApi({ "GET /api/teams": { body: [teamPayments, teamEmpty] }, ...base });
    renderApp("/admin/teams");
    expect(await screen.findByRole("heading", { name: "Teams" })).toBeInTheDocument();
    await screen.findByRole("table");
    expect(screen.getAllByRole("columnheader").map((h) => h.textContent).map((t) => t?.replace("▲", ""))).toEqual(["Team", "Repositories", "Owner of", "Members"]);
    const rows = screen.getAllByRole("row");
    const payments = within(rows.find((r) => within(r).queryByRole("link", { name: "Payments" }) !== null)!);
    expect(payments.getByRole("link", { name: "Payments" })).toHaveAttribute("href", "/teams/2");
    const cells = payments.getAllByRole("cell").map((c) => c.textContent);
    expect(cells).toEqual(["Payments", "1", "1", "1"]);
    expect(screen.getByRole("table")).not.toHaveTextContent("dev-user@example.com");
    expect(screen.getByRole("table")).not.toHaveTextContent("acme/payments");
    expect(screen.getByRole("table")).toHaveTextContent("Platform");
    expect(screen.queryByRole("button", { name: "Edit" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Remove" })).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "+ New team" })).toBeInTheDocument();
    // The name is asked in a dialog, not a form that is always open.
    expect(screen.queryByLabelText("Team name")).not.toBeInTheDocument();
  });

  it("counts a whole instance on its own, from the entries", async () => {
    const team = { ...teamPayments, repos: [{ instance: "main", project: "", owner: false }, ...teamPayments.repos] };
    fakeApi({ "GET /api/teams": { body: [team] }, ...base });
    renderApp("/admin/teams");
    await screen.findByRole("table");
    expect(screen.getByText("+1 instance")).toBeInTheDocument();
    expect(screen.getByRole("table")).toHaveTextContent("Payments1+1 instance11");
  });

  it("searches by name, says so when nothing matches, and sorts by Members", async () => {
    const big = { ...teamEmpty, id: 4, name: "Zed", members: [teamPayments.members[0], teamPayments.members[0]] };
    fakeApi({ "GET /api/teams": { body: [teamPayments, teamEmpty, big] }, ...base });
    renderApp("/admin/teams");
    await screen.findByRole("table");
    const names = () =>
      screen
        .getAllByRole("row")
        .slice(1)
        .map((r) => within(r).getAllByRole("cell")[0].textContent);
    expect(names()).toEqual(["Payments", "Platform", "Zed"]);
    await userEvent.click(screen.getByRole("button", { name: "Members" }));
    expect(names()).toEqual(["Zed", "Payments", "Platform"]);
    await userEvent.type(screen.getByLabelText("Search teams"), "zzz");
    expect(await screen.findByText(/No team matches\./)).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Clear" }));
    await vi.waitFor(() => expect(names()).toHaveLength(3));
  });

  it("creates a team from a name alone", async () => {
    let listed = 0;
    const fake = fakeApi({
      "GET /api/teams": () => ({ body: listed++ === 0 ? [] : [teamPayments] }),
      "POST /api/teams": { status: 201, body: teamPayments },
      "GET /api/teams/2": { body: teamPagePayments },
      ...base,
    });
    renderApp("/admin/teams");
    await userEvent.click(await screen.findByRole("button", { name: "+ New team" }));
    const dialog = screen.getByRole("dialog", { name: "New team" });
    expect(within(dialog).getByLabelText("Team name")).toHaveFocus();
    await userEvent.type(within(dialog).getByLabelText("Team name"), "Payments");
    expect(screen.queryByLabelText("Role")).not.toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Create team" }));
    const [post] = fake.find("POST", "/api/teams");
    expect(post.body).toEqual({ name: "Payments" });
    expect(post.headers.get("X-CSRF-Token")).toBe("csrf-123");
    await vi.waitFor(() => expect(screen.getByTestId("location")).toHaveTextContent("/teams/2?tab=members"));
  });

  it("shows the server's text when the name is taken", async () => {
    fakeApi({
      "GET /api/teams": { body: [] },
      "POST /api/teams": { status: 409, body: { error: "a team with this name exists already" } },
      ...base,
    });
    renderApp("/admin/teams");
    await userEvent.click(await screen.findByRole("button", { name: "+ New team" }));
    await userEvent.type(screen.getByLabelText("Team name"), "Payments");
    await userEvent.click(screen.getByRole("button", { name: "Create team" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("a team with this name exists already");
    // The dialog stays open, with the name, to fix it.
    expect(screen.getByLabelText("Team name")).toHaveValue("Payments");
  });

  it("closes the New team dialog with Cancel or Escape, and gives the focus back to the button", async () => {
    const fake = fakeApi({ "GET /api/teams": { body: [teamPayments] }, ...base });
    renderApp("/admin/teams");
    const open = await screen.findByRole("button", { name: "+ New team" });
    await userEvent.click(open);
    await userEvent.click(within(screen.getByRole("dialog", { name: "New team" })).getByRole("button", { name: "Cancel" }));
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    // The dialog moves the focus back on the next tick, once it is gone.
    await vi.waitFor(() => expect(open).toHaveFocus());
    await userEvent.click(open);
    await userEvent.type(screen.getByLabelText("Team name"), "Gone");
    await userEvent.keyboard("{Escape}");
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    await vi.waitFor(() => expect(open).toHaveFocus());
    expect(fake.find("POST", "/api/teams")).toHaveLength(0);
  });
});
