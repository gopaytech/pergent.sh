import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { fakeApi } from "../test/fakeApi";
import {
  instanceDashboardGitLab,
  instanceGitLab,
  operator,
  platformAdmin,
  repoPayments,
  repoWidgets,
  teamAdmins,
  teamPayments,
  userDev,
  userOperator,
  userWaiting,
} from "../test/fixtures";
import { renderApp } from "../test/render";

afterEach(() => vi.unstubAllGlobals());

const base = {
  "GET /api/users": { body: [userWaiting, userDev, userOperator] },
  "GET /api/repositories": { body: [repoPayments, repoWidgets] },
};

const teamCard = (name: string) => screen.getByRole("region", { name }) as HTMLElement;

describe("Teams", () => {
  it("lists the teams with their role, members and repositories", async () => {
    fakeApi({ "GET /api/me": { body: platformAdmin }, "GET /api/teams": { body: [teamPayments, teamAdmins] }, ...base });
    renderApp("/teams");
    expect(await screen.findByRole("heading", { name: "Teams" })).toBeInTheDocument();
    const payments = await screen.findByRole("region", { name: "Payments" });
    expect(payments).toHaveTextContent("maintainer");
    expect(payments).toHaveTextContent("dev-user");
    expect(payments).toHaveTextContent("acme/payments");
    const admins = teamCard("Admins");
    expect(admins).toHaveTextContent("Admins see every repository they can see on their platform.");
  });

  it("creates a team from a name and a role", async () => {
    let listed = 0;
    const fake = fakeApi({
      "GET /api/me": { body: platformAdmin },
      "GET /api/teams": () => ({ body: listed++ === 0 ? [] : [teamPayments] }),
      "POST /api/teams": { status: 201, body: teamPayments },
      ...base,
    });
    renderApp("/teams");
    await userEvent.type(await screen.findByLabelText("Team name"), "Payments");
    await userEvent.selectOptions(screen.getByLabelText("Role"), "maintainer");
    await userEvent.click(screen.getByRole("button", { name: "Create team" }));
    const [post] = fake.find("POST", "/api/teams");
    expect(post.body).toEqual({ name: "Payments", role: "maintainer" });
    expect(post.headers.get("X-CSRF-Token")).toBe("csrf-123");
    expect(await screen.findByRole("region", { name: "Payments" })).toBeInTheDocument();
    expect(screen.getByLabelText("Team name")).toHaveValue("");
  });

  it("shows the server's text when the name is taken", async () => {
    fakeApi({
      "GET /api/me": { body: platformAdmin },
      "GET /api/teams": { body: [] },
      "POST /api/teams": { status: 409, body: { error: "a team with this name exists already" } },
      ...base,
    });
    renderApp("/teams");
    await userEvent.type(await screen.findByLabelText("Team name"), "Payments");
    await userEvent.click(screen.getByRole("button", { name: "Create team" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("a team with this name exists already");
  });

  it("hides the repositories of an admin team", async () => {
    fakeApi({ "GET /api/me": { body: platformAdmin }, "GET /api/teams": { body: [teamAdmins] }, ...base });
    renderApp("/teams");
    await userEvent.click(within(await screen.findByRole("region", { name: "Admins" })).getByRole("button", { name: "Edit" }));
    const editor = screen.getByRole("form", { name: "Edit Admins" });
    expect(within(editor).getByText("Admins see every repository they can see on their platform.")).toBeInTheDocument();
    expect(within(editor).queryByRole("group", { name: "Repositories" })).not.toBeInTheDocument();
    expect(within(editor).getByRole("group", { name: "Members" })).toBeInTheDocument();
  });

  it("hides the repositories when the role is changed to admin, and sends none", async () => {
    const fake = fakeApi({
      "GET /api/me": { body: platformAdmin },
      "GET /api/teams": { body: [teamPayments] },
      "PUT /api/teams/2": { body: { ...teamPayments, role: "admin", repos: [] } },
      ...base,
    });
    renderApp("/teams");
    await userEvent.click(within(await screen.findByRole("region", { name: "Payments" })).getByRole("button", { name: "Edit" }));
    const editor = screen.getByRole("form", { name: "Edit Payments" });
    expect(within(editor).getByRole("group", { name: "Repositories" })).toBeInTheDocument();
    await userEvent.selectOptions(within(editor).getByLabelText("Role"), "admin");
    expect(within(editor).queryByRole("group", { name: "Repositories" })).not.toBeInTheDocument();
    await userEvent.click(within(editor).getByRole("button", { name: "Save" }));
    const [put] = fake.find("PUT", "/api/teams/2");
    expect(put.body).toMatchObject({ role: "admin", repos: [] });
  });

  it("says how many repositories the team covers that the admin can't see", async () => {
    fakeApi({
      "GET /api/me": { body: platformAdmin },
      "GET /api/teams": { body: [{ ...teamPayments, hiddenRepos: 3 }, { ...teamAdmins, hiddenRepos: 0 }] },
      ...base,
    });
    renderApp("/teams");
    const card = await screen.findByRole("region", { name: "Payments" });
    await userEvent.click(within(card).getByRole("button", { name: "Edit" }));
    expect(screen.getByRole("form", { name: "Edit Payments" })).toHaveTextContent("+3 repositories you can't see");
    expect(teamCard("Admins")).not.toHaveTextContent("you can't see");
  });

  it("doesn't promise to keep hidden repositories once the role is admin", async () => {
    fakeApi({
      "GET /api/me": { body: platformAdmin },
      "GET /api/teams": { body: [{ ...teamPayments, hiddenRepos: 3 }] },
      ...base,
    });
    renderApp("/teams");
    await userEvent.click(within(await screen.findByRole("region", { name: "Payments" })).getByRole("button", { name: "Edit" }));
    const editor = screen.getByRole("form", { name: "Edit Payments" });
    expect(editor).toHaveTextContent("They stay as they are when you save.");
    await userEvent.selectOptions(within(editor).getByLabelText("Role"), "admin");
    expect(editor).toHaveTextContent("+3 repositories you can't see");
    expect(editor).not.toHaveTextContent("They stay as they are");
    expect(editor).toHaveTextContent("can't be made an admin team");
  });

  it("shows a covered repository that is no longer switched on as such, unticked", async () => {
    const fake = fakeApi({
      "GET /api/me": { body: platformAdmin },
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

  it("saves the name, role, members and repositories with a PUT, keeping what it doesn't show", async () => {
    const fake = fakeApi({
      "GET /api/me": { body: platformAdmin },
      "GET /api/teams": { body: [{ ...teamPayments, hiddenRepos: 2 }] },
      "PUT /api/teams/2": { body: teamPayments },
      ...base,
    });
    renderApp("/teams");
    await userEvent.click(within(await screen.findByRole("region", { name: "Payments" })).getByRole("button", { name: "Edit" }));
    const editor = screen.getByRole("form", { name: "Edit Payments" });
    const members = within(editor).getByRole("group", { name: "Members" });
    // Only accounts that sign in with GitLab or GitHub can be members.
    expect(within(members).queryByRole("checkbox", { name: /admin/ })).not.toBeInTheDocument();
    expect(within(members).getByRole("checkbox", { name: /dev-user/ })).toBeChecked();
    await userEvent.click(within(members).getByRole("checkbox", { name: /newcomer/ }));
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
      role: "maintainer",
      members: [5, 7],
      repos: [
        { instance: "main", project: "acme/payments" },
        { instance: "main", project: "acme/widgets" },
      ],
    });
    // Saved: the editor closes.
    await vi.waitFor(() => expect(screen.queryByRole("form", { name: "Edit Payments" })).not.toBeInTheDocument());
  });

  it("covers a whole instance with one choice, for a platform admin its own", async () => {
    const fake = fakeApi({
      "GET /api/me": { body: platformAdmin },
      "GET /api/teams": { body: [teamPayments] },
      "PUT /api/teams/2": { body: teamPayments },
      ...base,
    });
    renderApp("/teams");
    await userEvent.click(within(await screen.findByRole("region", { name: "Payments" })).getByRole("button", { name: "Edit" }));
    const editor = screen.getByRole("form", { name: "Edit Payments" });
    await userEvent.click(within(editor).getByRole("checkbox", { name: "All repositories of main" }));
    await userEvent.click(within(editor).getByRole("button", { name: "Save" }));
    const [put] = fake.find("PUT", "/api/teams/2");
    expect((put.body as { repos: unknown[] }).repos).toEqual([
      { instance: "main", project: "" },
      { instance: "main", project: "acme/payments" },
    ]);
    expect(fake.find("GET", "/api/admin/instances")).toHaveLength(0);
  });

  it("offers an operator every instance as a whole", async () => {
    fakeApi({
      "GET /api/me": { body: operator },
      "GET /api/teams": { body: [teamPayments] },
      "GET /api/admin/instances": { body: [instanceGitLab, instanceDashboardGitLab] },
      ...base,
    });
    renderApp("/teams");
    await userEvent.click(within(await screen.findByRole("region", { name: "Payments" })).getByRole("button", { name: "Edit" }));
    const editor = screen.getByRole("form", { name: "Edit Payments" });
    expect(await within(editor).findByRole("checkbox", { name: "All repositories of main" })).toBeInTheDocument();
    expect(within(editor).getByRole("checkbox", { name: "All repositories of extra" })).toBeInTheDocument();
  });

  it("shows the server's text when a save is refused, and stays open", async () => {
    fakeApi({
      "GET /api/me": { body: platformAdmin },
      "GET /api/teams": { body: [teamPayments] },
      "PUT /api/teams/2": {
        status: 409,
        body: { error: "this team covers repositories you can't see; a password admin can change its role or remove it" },
      },
      ...base,
    });
    renderApp("/teams");
    await userEvent.click(within(await screen.findByRole("region", { name: "Payments" })).getByRole("button", { name: "Edit" }));
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("a password admin can change its role or remove it");
    expect(screen.getByRole("form", { name: "Edit Payments" })).toBeInTheDocument();
  });

  it("removes a team after a confirmation", async () => {
    const confirm = vi.fn(() => true);
    vi.stubGlobal("confirm", confirm);
    let listed = 0;
    const fake = fakeApi({
      "GET /api/me": { body: platformAdmin },
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
