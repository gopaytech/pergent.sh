import { describe, expect, it } from "vitest";

import { membersText, teamCountsText } from "./teamText";

describe("teamCountsText", () => {
  const cases: [string, { repoCount: number; instanceCount: number; ownedCount: number }, string][] = [
    ["all parts", { repoCount: 9, instanceCount: 1, ownedCount: 9 }, "9 repositories, 1 whole instance, owner of 9"],
    ["one of each", { repoCount: 1, instanceCount: 1, ownedCount: 1 }, "1 repository, 1 whole instance, owner of 1"],
    ["many instances", { repoCount: 0, instanceCount: 2, ownedCount: 0 }, "2 whole instances"],
    ["repositories only", { repoCount: 3, instanceCount: 0, ownedCount: 0 }, "3 repositories"],
    ["owner only", { repoCount: 0, instanceCount: 0, ownedCount: 2 }, "owner of 2"],
    ["all zero", { repoCount: 0, instanceCount: 0, ownedCount: 0 }, "0 repositories"],
  ];
  it.each(cases)("%s", (_name, counts, want) => {
    expect(teamCountsText(counts)).toBe(want);
  });
});

describe("membersText", () => {
  it.each([
    [0, "0 members"],
    [1, "1 member"],
    [4, "4 members"],
  ])("%d", (n, want) => {
    expect(membersText(n)).toBe(want);
  });
});
