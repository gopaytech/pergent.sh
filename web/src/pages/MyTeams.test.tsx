import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import type { MyTeamView } from "../api";
import { fakeApi } from "../test/fakeApi";
import { maintainer } from "../test/fixtures";
import { renderApp } from "../test/render";

const big: MyTeamView = {
  id: 7,
  name: "Big",
  role: "admin",
  repos: [
    { instance: "main", project: "acme/secret-one", owner: true },
    { instance: "main", project: "", owner: false },
  ],
  memberCount: 6,
  repoCount: 4,
  instanceCount: 1,
  ownedCount: 4,
};

const pay: MyTeamView = { id: 8, name: "Pay", role: "viewer", repos: [], memberCount: 9, repoCount: 2, instanceCount: 0, ownedCount: 0 };
const ops: MyTeamView = { id: 9, name: "Ops", role: "maintainer", repos: [], memberCount: 3, repoCount: 0, instanceCount: 2, ownedCount: 0 };

const routes = (teams: MyTeamView[]) => ({ "GET /api/me": { body: maintainer }, "GET /api/me/teams": { body: teams } });
const names = () =>
  screen
    .getAllByRole("row")
    .slice(1)
    .map((r) => within(r).getAllByRole("cell")[0].textContent);

describe("MyTeams", () => {
  it("lists the teams in a table with counts, not repository names, and links the name to the team page", async () => {
    fakeApi(routes([big]));
    renderApp("/teams");
    await screen.findByRole("table");
    expect(screen.getAllByRole("columnheader").map((h) => h.textContent).map((t) => t?.replace("▲", ""))).toEqual(["Team", "Your role", "Repositories", "Owner of", "Members"]);
    const cells = within(screen.getAllByRole("row")[1]).getAllByRole("cell");
    expect(cells[0]).toHaveTextContent("Big");
    expect(cells[1]).toHaveTextContent("admin");
    expect(cells[2]).toHaveTextContent("4");
    expect(cells[3]).toHaveTextContent("4");
    expect(cells[4]).toHaveTextContent("6");
    expect(screen.getByRole("table")).not.toHaveTextContent("acme/secret-one");
    expect(screen.getByRole("link", { name: "Big" })).toHaveAttribute("href", "/teams/7");
  });

  it("shows a whole-instance count under the Repositories number", async () => {
    fakeApi(routes([big, pay, ops]));
    renderApp("/teams");
    await screen.findByRole("table");
    expect(screen.getByText("+1 instance")).toBeInTheDocument();
    expect(screen.getByText("+2 instances")).toBeInTheDocument();
    expect(screen.getAllByText(/^\+\d instances?$/)).toHaveLength(2);
  });

  it("searches by name, and says so when nothing matches", async () => {
    fakeApi(routes([big, pay]));
    renderApp("/teams");
    await userEvent.type(await screen.findByLabelText("Search teams"), "pay");
    await vi.waitFor(() => expect(names()).toEqual(["Pay"]));
    expect(screen.getByText("1 of 2 shown")).toBeInTheDocument();
    await userEvent.clear(screen.getByLabelText("Search teams"));
    await userEvent.type(screen.getByLabelText("Search teams"), "zzz");
    expect(await screen.findByText(/No team matches\./)).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Clear" }));
    await vi.waitFor(() => expect(names()).toEqual(["Big", "Pay"]));
  });

  it("sorts by Members, most first", async () => {
    fakeApi(routes([big, pay, ops]));
    renderApp("/teams");
    await screen.findByRole("table");
    expect(names()).toEqual(["Big", "Ops", "Pay"]);
    await userEvent.click(screen.getByRole("button", { name: "Members" }));
    expect(names()).toEqual(["Pay", "Big", "Ops"]);
  });

  it("says so when the user is on no team", async () => {
    fakeApi(routes([]));
    renderApp("/teams");
    expect(await screen.findByText(/You are on no team yet/)).toBeInTheDocument();
  });
});
