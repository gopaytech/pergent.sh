import { describe, expect, it } from "vitest";

import type { ChainRef } from "./api";
import { costHint, firstJudge, removeReviewer, type ReviewersDraft } from "./reviewers";

const ref = (id: string): ChainRef => ({ source: "global", id });

describe("costHint", () => {
  it("is empty for 1 reviewer", () => {
    expect(costHint(1)).toBe("");
  });

  it("counts the reviews and the judge for 2 reviewers", () => {
    expect(costHint(2)).toBe("Each review now runs 2 reviews and 1 judge, about 2 to 3 times the cost of one review.");
  });

  it("counts the reviews and the judge for 5 reviewers", () => {
    expect(costHint(5)).toContain("runs 5 reviews and 1 judge, about 5 to 6 times");
  });
});

describe("firstJudge", () => {
  it("is reviewer 2's first entry", () => {
    expect(firstJudge([ref("m-opus"), ref("m-sonnet")])).toEqual([ref("m-opus")]);
  });

  it("is empty for an empty reviewer", () => {
    expect(firstJudge([])).toEqual([]);
  });
});

describe("removeReviewer", () => {
  const four: ReviewersDraft = {
    reviewers: [[ref("r2")], [ref("r3")], [ref("r4")]],
    judge: [ref("j")],
  };

  it("moves the later reviewers up one number and keeps the judge", () => {
    // Removing reviewer 2 of 4: reviewer 3 is now 2, and 4 is now 3.
    expect(removeReviewer(four, 2)).toEqual({ reviewers: [[ref("r3")], [ref("r4")]], judge: [ref("j")] });
  });

  it("clears the judge when no extra reviewer is left", () => {
    expect(removeReviewer({ reviewers: [[ref("r2")]], judge: [ref("j")] }, 2)).toEqual({ reviewers: [], judge: [] });
  });

  it("does not change the draft it is given", () => {
    removeReviewer(four, 3);
    expect(four.reviewers).toHaveLength(3);
  });
});
