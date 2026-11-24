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

const teamCard = (name: string) => screen.getByRole("region", { name }) as HTMLElement;

describe("Teams", () => {
  it("lists each team with its name linked to the team page, its member count and its repository count", async () => {
    fakeApi({ "GET /api/teams": { body: [teamPayments, teamEmpty] }, ...base });
    renderApp("/admin/teams");
    expect(await screen.findByRole("heading", { name: "Teams" })).toBeInTheDocument();
    const payments = await screen.findByRole("region", { name: "Payments" });
    expect(within(payments).getByRole("link", { name: "Payments" })).toHaveAttribute("href", "/teams/2");
    expect(payments).toHaveTextContent("1 member");
    expect(payments).toHaveTextContent("1 repository, owner of 1");
    expect(payments).not.toHaveTextContent("dev-user@example.com");
    expect(payments).not.toHaveTextContent("acme/payments");
    expect(teamCard("Platform")).toHaveTextContent("0 members");
    expect(teamCard("Platform")).toHaveTextContent("0 repositories");
    expect(screen.queryByRole("button", { name: "Edit" })).not.toBeInTheDocument();
  });

  it("counts a whole instance on its own, from the entries", async () => {
    const team = { ...teamPayments, repos: [{ instance: "main", project: "", owner: false }, ...teamPayments.repos] };
    fakeApi({ "GET /api/teams": { body: [team] }, ...base });
    renderApp("/admin/teams");
    expect(await screen.findByRole("region", { name: "Payments" })).toHaveTextContent("1 repository, 1 whole instance, owner of 1");
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
    await userEvent.type(await screen.findByLabelText("Team name"), "Payments");
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
    await userEvent.type(await screen.findByLabelText("Team name"), "Payments");
    await userEvent.click(screen.getByRole("button", { name: "Create team" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("a team with this name exists already");
  });

  it("removes a team after a confirmation", async () => {
    const confirm = vi.fn(() => true);
    vi.stubGlobal("confirm", confirm);
    let listed = 0;
    const fake = fakeApi({
      "GET /api/teams": () => ({ body: listed++ === 0 ? [teamPayments] : [] }),
      "DELETE /api/teams/2": { status: 204 },
      ...base,
    });
    renderApp("/admin/teams");
    await userEvent.click(within(await screen.findByRole("region", { name: "Payments" })).getByRole("button", { name: "Remove" }));
    expect(confirm).toHaveBeenCalledWith(expect.stringContaining("Payments"));
    expect(fake.find("DELETE", "/api/teams/2")[0].headers.get("X-CSRF-Token")).toBe("csrf-123");
    await vi.waitFor(() => expect(screen.queryByRole("region", { name: "Payments" })).not.toBeInTheDocument());
  });
});
