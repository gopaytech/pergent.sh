import { describe, expect, it } from "vitest";

import { teamPagePayments } from "../test/fixtures";
import { teamUpdate } from "./teamSave";

const v = teamPagePayments;
// What the page read, as the PUT body sends it: the removed entry is left out.
const reposAsRead = [
  { instance: "main", project: "acme/payments", owner: true },
  { instance: "side", project: "", owner: false },
];
const membersAsRead = [
  { accountId: 11, role: "maintainer" },
  { accountId: 12, role: "viewer" },
];

describe("teamUpdate", () => {
  it("members change keeps repos as read", () => {
    const members = [{ accountId: 11, role: "viewer" as const }];
    expect(teamUpdate(v, { members })).toEqual({ name: "Payments", members, repos: reposAsRead });
  });

  it("repos change keeps members as read", () => {
    const repos = [{ instance: "main", project: "acme/payments", owner: false }];
    expect(teamUpdate(v, { repos })).toEqual({ name: "Payments", members: membersAsRead, repos });
  });

  it("removed entries are dropped from a passed repos list too", () => {
    const repos = [
      { instance: "main", project: "acme/old", owner: true },
      { instance: "main", project: "acme/payments", owner: true },
    ];
    expect(teamUpdate(v, { repos }).repos).toEqual([{ instance: "main", project: "acme/payments", owner: true }]);
  });

  it("name defaults to v.team.name", () => {
    expect(teamUpdate(v, {}).name).toBe("Payments");
    expect(teamUpdate(v, { name: "Billing" }).name).toBe("Billing");
  });
});
