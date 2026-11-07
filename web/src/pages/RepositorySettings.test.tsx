import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { fakeApi } from "../test/fakeApi";
import { librarySkills, repoDetail, repoPayments, repoTeams, repoWidgets, ssoAdmin, viewer } from "../test/fixtures";
import { renderApp } from "../test/render";

afterEach(() => vi.unstubAllGlobals());

const noInputs = { branch: "main", skills: [], detected: [], instructions: "" };

const base = {
  "GET /api/me": { body: ssoAdmin },
  "GET /api/repositories/main/acme/payments": { body: repoDetail(repoPayments) },
  "GET /api/repositories/main/acme/widgets": { body: repoDetail(repoWidgets) },
  "GET /api/skills": { body: librarySkills },
  "GET /api/repositories/main/acme/payments/inputs": { body: noInputs },
  "GET /api/repositories/main/acme/widgets/inputs": { body: noInputs },
  "GET /api/repositories/main/acme/payments/teams": { body: repoTeams },
  "GET /api/repositories/main/acme/widgets/teams": { body: repoTeams },
};

const teamsSection = () => screen.findByRole("region", { name: "Teams" });

describe("RepositorySettings: Teams", () => {
  it("shows an admin every team, ticked when it lists the repository, and the whole-instance ones as such", async () => {
    fakeApi(base);
    renderApp("/repos/main/acme/payments?tab=settings");
    const box = await teamsSection();
    await within(box).findByRole("checkbox", { name: /Payments/ });
    expect(within(box).getByRole("checkbox", { name: /Payments/ })).toBeChecked();
    expect(within(box).getByRole("checkbox", { name: /Platform/ })).not.toBeChecked();
    const everyone = within(box).getByRole("checkbox", { name: /Everyone on main/ });
    expect(everyone).not.toBeChecked();
    expect(box).toHaveTextContent("covers every repository of main");
    expect(box).toHaveTextContent("6 members");
  });

  it("saves the ticked teams with a PUT and says so", async () => {
    const saved = { teams: repoTeams.teams.map((t) => (t.id === 3 ? { ...t, direct: true } : t)) };
    const fake = fakeApi({ ...base, "PUT /api/repositories/main/acme/payments/teams": { body: saved } });
    renderApp("/repos/main/acme/payments?tab=settings");
    const box = await teamsSection();
    await userEvent.click(await within(box).findByRole("checkbox", { name: /Platform/ }));
    await userEvent.click(within(box).getByRole("button", { name: "Save teams" }));
    const [put] = fake.find("PUT", "/api/repositories/main/acme/payments/teams");
    expect(put.body).toEqual({ teams: [2, 3] });
    expect(put.headers.get("X-CSRF-Token")).toBe("csrf-123");
    expect(await within(box).findByRole("status")).toHaveTextContent("Saved.");
    expect(within(box).getByRole("checkbox", { name: /Platform/ })).toBeChecked();
  });

  it("can take the repository off every team", async () => {
    const fake = fakeApi({
      ...base,
      "PUT /api/repositories/main/acme/payments/teams": { body: { teams: repoTeams.teams.map((t) => ({ ...t, direct: false })) } },
    });
    renderApp("/repos/main/acme/payments?tab=settings");
    const box = await teamsSection();
    await userEvent.click(await within(box).findByRole("checkbox", { name: /Payments/ }));
    await userEvent.click(within(box).getByRole("button", { name: "Save teams" }));
    expect(fake.find("PUT", "/api/repositories/main/acme/payments/teams")[0].body).toEqual({ teams: [] });
  });

  it("shows the server's text when a save is refused", async () => {
    fakeApi({
      ...base,
      "PUT /api/repositories/main/acme/payments/teams": { status: 400, body: { error: "every team must be a team that exists" } },
    });
    renderApp("/repos/main/acme/payments?tab=settings");
    const box = await teamsSection();
    await userEvent.click(await within(box).findByRole("checkbox", { name: /Platform/ }));
    await userEvent.click(within(box).getByRole("button", { name: "Save teams" }));
    expect(await within(box).findByRole("alert")).toHaveTextContent("every team must be a team that exists");
  });

  it("is there for a repository the config sets up, whose settings are locked", async () => {
    fakeApi(base);
    renderApp("/repos/main/acme/widgets?tab=settings");
    const box = await teamsSection();
    expect(await within(box).findByRole("checkbox", { name: /Payments/ })).toBeEnabled();
    expect(screen.getByText(/listed in pergent.sh's config/)).toBeInTheDocument();
  });

  it("points to Teams when there is none", async () => {
    fakeApi({ ...base, "GET /api/repositories/main/acme/payments/teams": { body: { teams: [] } } });
    renderApp("/repos/main/acme/payments?tab=settings");
    const box = await teamsSection();
    expect(await within(box).findByText(/No team yet/)).toBeInTheDocument();
    expect(within(box).getByRole("link", { name: /Teams/ })).toHaveAttribute("href", "/admin/teams");
  });

  it("is absent for a maintainer, and nothing asks for the teams", async () => {
    const fake = fakeApi({
      ...base,
      "GET /api/me": { body: viewer },
      "GET /api/repositories/main/acme/payments": { body: repoDetail(repoPayments, "maintainer") },
    });
    renderApp("/repos/main/acme/payments?tab=settings");
    await screen.findByRole("switch", { name: "Approve clean reviews" });
    expect(screen.queryByRole("region", { name: "Teams" })).not.toBeInTheDocument();
    expect(fake.calls.filter((c) => c.url.endsWith("/teams"))).toHaveLength(0);
  });
});
