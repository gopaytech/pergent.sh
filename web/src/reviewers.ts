import type { ChainRef, ChainView, ReviewersUpdate, ReviewersView } from "./api";

// ReviewersDraft is what the Reviewers card sends in one save: reviewers 2
// to 5 in order, each its entries, and the judge's entries.
export type ReviewersDraft = ReviewersUpdate;

// refsOf is the entries of a chain as a save sends them; none for no chain.
export const refsOf = (c: ChainView | null | undefined): ChainRef[] => (c ? c.entries.map((e) => ({ source: e.source, id: e.id })) : []);

// draftOf is the reviewers and judge the server has, as a save sends them.
export const draftOf = (v: ReviewersView): ReviewersDraft => ({
  reviewers: v.reviewers.map((r) => refsOf(r.chain)),
  judge: refsOf(v.judge),
});

// costHint is the line under the card: "" for 1 reviewer. Each reviewer runs
// one review, and the judge runs one more.
export function costHint(reviewers: number): string {
  if (reviewers < 2) return "";
  return `Each review now runs ${reviewers} reviews and 1 judge, about ${reviewers} to ${reviewers + 1} times the cost of one review.`;
}

// firstJudge is what the editor puts in the judge when reviewer 2 is added:
// reviewer 2's first entry.
export function firstJudge(r2: ChainRef[]): ChainRef[] {
  return r2.slice(0, 1).map((e) => ({ ...e }));
}

// removeReviewer drops reviewer n (2..5); later ones move up; the judge is
// cleared when no extra reviewer is left.
export function removeReviewer(s: ReviewersDraft, n: number): ReviewersDraft {
  const reviewers = s.reviewers.filter((_, i) => i !== n - 2);
  return { reviewers, judge: reviewers.length === 0 ? [] : s.judge };
}

// addReviewer puts a new reviewer at the end. A judge with no entries gets
// reviewer 2's first entry, since 2 or more reviewers need a judge.
export function addReviewer(s: ReviewersDraft, entries: ChainRef[]): ReviewersDraft {
  const reviewers = [...s.reviewers, entries];
  return { reviewers, judge: s.judge.length > 0 ? s.judge : firstJudge(reviewers[0]) };
}

// setReviewer replaces the entries of reviewer n (2..5).
export function setReviewer(s: ReviewersDraft, n: number, entries: ChainRef[]): ReviewersDraft {
  return { ...s, reviewers: s.reviewers.map((r, i) => (i === n - 2 ? entries : r)) };
}
