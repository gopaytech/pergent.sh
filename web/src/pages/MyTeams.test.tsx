import { screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import type { MyTeamView } from "../api";
import { fakeApi } from "../test/fakeApi";
import { maintainer } from "../test/fixtures";
import { renderApp } from "../test/render";

const big: MyTeamView = {
  id: 7,
  name: "Big",
  role: "maintainer",
  repos: [
    { instance: "main", project: "acme/secret-one", owner: true },
    { instance: "main", project: "", owner: false },
  ],
  memberCount: 4,
  repoCount: 9,
  instanceCount: 1,
  ownedCount: 9,
};

describe("MyTeams", () => {
  it("shows counts, not repository names, and links the name to the team page", async () => {
    fakeApi({ "GET /api/me": { body: maintainer }, "GET /api/me/teams": { body: [big] } });
    renderApp("/teams");
    const card = await screen.findByRole("region", { name: "Big" });
    expect(card).toHaveTextContent("9 repositories, 1 whole instance, owner of 9");
    expect(card).toHaveTextContent("4 members");
    expect(card).toHaveTextContent("Your role: maintainer");
    expect(card).not.toHaveTextContent("acme/secret-one");
    expect(screen.getByRole("link", { name: "Big" })).toHaveAttribute("href", "/teams/7");
  });
});
