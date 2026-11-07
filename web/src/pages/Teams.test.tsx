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
  it("lists the teams with their members, each with a role, and repositories", async () => {
    fakeApi({ "GET /api/teams": { body: [teamPayments, teamEmpty] }, ...base });
    renderApp("/teams");
    expect(await screen.findByRole("heading", { name: "Teams" })).toBeInTheDocument();
    const payments = await screen.findByRole("region", { name: "Payments" });
    expect(payments).toHaveTextContent("dev-user@example.com (maintainer)");
    expect(payments).toHaveTextContent("acme/payments");
    expect(teamCard("Platform")).toHaveTextContent("Members: none");
    expect(teamCard("Platform")).toHaveTextContent("Repositories: none");
  });

  it("creates a team from a name alone", async () => {
    let listed = 0;
    const fake = fakeApi({
      "GET /api/teams": () => ({ body: listed++ === 0 ? [] : [teamPayments] }),
      "POST /api/teams": { status: 201, body: teamPayments },
      ...base,
    });
    renderApp("/teams");
    await userEvent.type(await screen.findByLabelText("Team name"), "Payments");
    expect(screen.queryByLabelText("Role")).not.toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Create team" }));
    const [post] = fake.find("POST", "/api/teams");
    expect(post.body).toEqual({ name: "Payments" });
    expect(post.headers.get("X-CSRF-Token")).toBe("csrf-123");
    expect(await screen.findByRole("region", { name: "Payments" })).toBeInTheDocument();
    expect(screen.getByLabelText("Team name")).toHaveValue("");
  });

  it("shows the server's text when the name is taken", async () => {
    fakeApi({
      "GET /api/teams": { body: [] },
      "POST /api/teams": { status: 409, body: { error: "a team with this name exists already" } },
      ...base,
    });
    renderApp("/teams");
    await userEvent.type(await screen.findByLabelText("Team name"), "Payments");
    await userEvent.click(screen.getByRole("button", { name: "Create team" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("a team with this name exists already");
  });

  it("shows a covered repository that is no longer switched on as such, unticked", async () => {
    const fake = fakeApi({
      "GET /api/teams": {
        body: [{ ...teamPayments, repos: [{ instance: "main", project: "acme/payments" }, { instance: "main", project: "acme/gone" }] }],
      },
      "PUT /api/teams/2": { body: teamPayments },
      ...base,
    });
    renderApp("/teams");
    await userEvent.click(within(await screen.findByRole("region", { name: "Payments" })).getByRole("button", { name: "Edit" }));
    const editor = screen.getByRole("form", { name: "Edit Payments" });
    const gone = await within(editor).findByRole("checkbox", { name: /acme\/gone.*switched off/ });
    expect(gone).not.toBeChecked();
    expect(within(editor).getByRole("checkbox", { name: /acme\/payments/ })).toBeChecked();
    await userEvent.click(within(editor).getByRole("button", { name: "Save" }));
    expect((fake.find("PUT", "/api/teams/2")[0].body as { repos: unknown[] }).repos).toEqual([
      { instance: "main", project: "acme/payments" },
    ]);
  });

  it("saves the name, members with their roles and repositories with a PUT", async () => {
    const fake = fakeApi({
      "GET /api/teams": { body: [teamPayments] },
      "PUT /api/teams/2": { body: teamPayments },
      ...base,
    });
    renderApp("/teams");
    await userEvent.click(within(await screen.findByRole("region", { name: "Payments" })).getByRole("button", { name: "Edit" }));
    const editor = screen.getByRole("form", { name: "Edit Payments" });
    expect(within(editor).queryByLabelText("Role")).not.toBeInTheDocument();
    const members = within(editor).getByRole("group", { name: "Members" });
    // Only SSO accounts can be members: a local admin is no member.
    expect(within(members).queryByRole("checkbox", { name: /admin/ })).not.toBeInTheDocument();
    expect(within(members).getByRole("checkbox", { name: /dev-user/ })).toBeChecked();
    expect(within(members).getByLabelText("Role of dev-user@example.com")).toHaveValue("maintainer");
    // A member added is a viewer until the role is changed.
    await userEvent.click(within(members).getByRole("checkbox", { name: /newcomer/ }));
    expect(within(members).getByLabelText("Role of newcomer@example.com")).toHaveValue("viewer");
    await userEvent.selectOptions(within(members).getByLabelText("Role of dev-user@example.com"), "viewer");
    const repos = within(editor).getByRole("group", { name: "Repositories" });
    expect(within(repos).getByRole("checkbox", { name: /acme\/payments/ })).toBeChecked();
    expect(within(repos).getByRole("checkbox", { name: /acme\/widgets/ })).not.toBeChecked();
    await userEvent.click(within(repos).getByRole("checkbox", { name: /acme\/widgets/ }));
    await userEvent.clear(within(editor).getByLabelText("Name"));
    await userEvent.type(within(editor).getByLabelText("Name"), "Payments crew");
    await userEvent.click(within(editor).getByRole("button", { name: "Save" }));

    const [put] = fake.find("PUT", "/api/teams/2");
    expect(put.headers.get("X-CSRF-Token")).toBe("csrf-123");
    expect(put.body).toEqual({
      name: "Payments crew",
      members: [
        { accountId: 5, role: "viewer" },
        { accountId: 7, role: "viewer" },
      ],
      repos: [
        { instance: "main", project: "acme/payments" },
        { instance: "main", project: "acme/widgets" },
      ],
    });
    // Saved: the editor closes.
    await vi.waitFor(() => expect(screen.queryByRole("form", { name: "Edit Payments" })).not.toBeInTheDocument());
  });

  it("offers every instance as a whole", async () => {
    const fake = fakeApi({
      "GET /api/teams": { body: [teamPayments] },
      "PUT /api/teams/2": { body: teamPayments },
      ...base,
    });
    renderApp("/teams");
    await userEvent.click(within(await screen.findByRole("region", { name: "Payments" })).getByRole("button", { name: "Edit" }));
    const editor = screen.getByRole("form", { name: "Edit Payments" });
    expect(await within(editor).findByRole("checkbox", { name: "All repositories of extra" })).toBeInTheDocument();
    await userEvent.click(within(editor).getByRole("checkbox", { name: "All repositories of main" }));
    await userEvent.click(within(editor).getByRole("button", { name: "Save" }));
    const [put] = fake.find("PUT", "/api/teams/2");
    expect((put.body as { repos: unknown[] }).repos).toEqual([
      { instance: "main", project: "" },
      { instance: "main", project: "acme/payments" },
    ]);
  });

  it("shows the server's text when a save is refused, and stays open", async () => {
    fakeApi({
      "GET /api/teams": { body: [teamPayments] },
      "PUT /api/teams/2": { status: 400, body: { error: "members must be accounts that sign in with SSO" } },
      ...base,
    });
    renderApp("/teams");
    await userEvent.click(within(await screen.findByRole("region", { name: "Payments" })).getByRole("button", { name: "Edit" }));
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("members must be accounts that sign in with SSO");
    expect(screen.getByRole("form", { name: "Edit Payments" })).toBeInTheDocument();
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
    renderApp("/teams");
    await userEvent.click(within(await screen.findByRole("region", { name: "Payments" })).getByRole("button", { name: "Remove" }));
    expect(confirm).toHaveBeenCalledWith(expect.stringContaining("Payments"));
    expect(fake.find("DELETE", "/api/teams/2")[0].headers.get("X-CSRF-Token")).toBe("csrf-123");
    await vi.waitFor(() => expect(screen.queryByRole("region", { name: "Payments" })).not.toBeInTheDocument());
  });
});
