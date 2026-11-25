import { screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { fakeApi } from "../test/fakeApi";
import { chainRepoInherit, librarySkills, repoDetail, repoPayments, repoTeams, reviewersNone, ssoAdmin } from "../test/fixtures";
import { renderApp } from "../test/render";

afterEach(() => vi.unstubAllGlobals());

const payments = "/api/repositories/main/acme/payments";
const routes = {
  "GET /api/me": { body: ssoAdmin },
  [`GET ${payments}`]: { body: repoDetail(repoPayments) },
  "GET /api/skills": { body: librarySkills },
  [`GET ${payments}/inputs`]: { body: { branch: "main", skills: [], detected: [], instructions: "" } },
  [`GET ${payments}/teams`]: { body: repoTeams },
  [`GET ${payments}/chain`]: { body: chainRepoInherit },
  [`GET ${payments}/reviewers`]: { body: reviewersNone },
};

describe("RepoModelChain: one Reviewers card", () => {
  it("puts the model chain in the Reviewers card as reviewer 1, with no other chain card", async () => {
    const fake = fakeApi(routes);
    renderApp("/repos/main/acme/payments?tab=settings", { unfolded: true });
    const box = await screen.findByRole("region", { name: "Reviewers" });
    const one = await within(box).findByRole("group", { name: "Reviewer 1" });
    expect(await within(one).findByText("qwen3.5")).toBeInTheDocument();
    expect(screen.queryByRole("region", { name: "Model chain" })).not.toBeInTheDocument();
    expect(screen.queryByRole("region", { name: /\bchain B\b/i })).not.toBeInTheDocument();
    expect(document.body).not.toHaveTextContent(/\bchain B\b/i);
    expect(fake.calls.filter((c) => c.url.includes("panel-chain"))).toHaveLength(0);
  });
});
