import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import type { ReviewersView } from "./api";
import { fakeApi } from "./test/fakeApi";
import {
  chainRepoInherit,
  entryConfig,
  entryOpus,
  entrySonnet,
  entryTeamQwen,
  librarySkills,
  repoDetail,
  repoPayments,
  repoTeams,
  reviewersNone,
  reviewersWith,
  ssoAdmin,
  viewer,
} from "./test/fixtures";
import { renderApp } from "./test/render";

afterEach(() => vi.unstubAllGlobals());

const payments = "/api/repositories/main/acme/payments";
const reviewersPath = `${payments}/reviewers`;
const routes = (reviewers: ReviewersView = reviewersNone, me = ssoAdmin) => ({
  "GET /api/me": { body: me },
  [`GET ${payments}`]: { body: repoDetail(repoPayments, me === ssoAdmin ? "admin" : "viewer") },
  "GET /api/skills": { body: librarySkills },
  [`GET ${payments}/inputs`]: { body: { branch: "main", skills: [], detected: [], instructions: "" } },
  [`GET ${payments}/teams`]: { body: repoTeams },
  [`GET ${payments}/chain`]: { body: chainRepoInherit },
  [`GET ${reviewersPath}`]: { body: reviewers },
});
const card = () => screen.findByRole("region", { name: "Reviewers" });
const group = (box: HTMLElement, name: string) => within(box).findByRole("group", { name });
const ref = (e: { source: string; id: string }) => ({ source: e.source, id: e.id });

// Reviewer 2 is Opus, judged by Opus.
const two = reviewersWith([[entryOpus]], [entryOpus]);
// Reviewers 2, 3 and 4: Opus, Sonnet, and the config file's entry with Opus as its backup.
const four = reviewersWith([[entryOpus], [entrySonnet], [entryConfig, entryOpus]], [entryOpus]);

describe("ReviewersCard", () => {
  it("shows reviewer 1 as the chain in effect, and no judge and no cost with 1 reviewer", async () => {
    fakeApi(routes());
    renderApp("/repos/main/acme/payments?tab=settings");
    const box = await card();
    const one = await group(box, "Reviewer 1");
    expect(await within(one).findByText("Inherited from the chain of its owner team, Payments.")).toBeInTheDocument();
    expect(within(box).queryByRole("group", { name: "Judge" })).not.toBeInTheDocument();
    expect(box).not.toHaveTextContent(/times the cost/);
  });

  it("adds reviewer 2, fills the judge with its first entry, and saves both in one PUT", async () => {
    const fake = fakeApi({ ...routes(), [`PUT ${reviewersPath}`]: { body: two } });
    renderApp("/repos/main/acme/payments?tab=settings");
    const box = await card();
    await userEvent.click(await within(box).findByRole("button", { name: "Add a reviewer" }));
    const r2 = await group(box, "Reviewer 2");
    await userEvent.selectOptions(within(r2).getByRole("combobox", { name: "Entry to add" }), `global:${entryOpus.id}`);
    await userEvent.click(within(r2).getByRole("button", { name: "Add" }));
    await userEvent.click(within(r2).getByRole("button", { name: "Save chain" }));
    const [put] = fake.find("PUT", reviewersPath);
    expect(put.headers.get("X-CSRF-Token")).toBe("csrf-123");
    expect(put.body).toEqual({ reviewers: [[ref(entryOpus)]], judge: [ref(entryOpus)] });
    expect(await within(box).findByText("(multi review, experimental)")).toBeInTheDocument();
    const judge = await group(box, "Judge");
    expect(within(judge).getByText(entryOpus.name)).toBeInTheDocument();
    expect(box).toHaveTextContent("Each review now runs 2 reviews and 1 judge, about 2 to 3 times the cost of one review.");
  });

  it("adds reviewer 3 and keeps the judge it has", async () => {
    const fake = fakeApi({ ...routes(two), [`PUT ${reviewersPath}`]: { body: two } });
    renderApp("/repos/main/acme/payments?tab=settings");
    const box = await card();
    await userEvent.click(await within(box).findByRole("button", { name: "Add a reviewer" }));
    const r3 = await group(box, "Reviewer 3");
    await userEvent.selectOptions(within(r3).getByRole("combobox", { name: "Entry to add" }), `global:${entrySonnet.id}`);
    await userEvent.click(within(r3).getByRole("button", { name: "Add" }));
    await userEvent.click(within(r3).getByRole("button", { name: "Save chain" }));
    expect(fake.find("PUT", reviewersPath)[0].body).toEqual({ reviewers: [[ref(entryOpus)], [ref(entrySonnet)]], judge: [ref(entryOpus)] });
  });

  it("removes reviewer 2 with a PUT of the shorter list, so the later reviewers move up", async () => {
    const fake = fakeApi({ ...routes(four), [`PUT ${reviewersPath}`]: { body: reviewersWith([[entrySonnet], [entryConfig, entryOpus]], [entryOpus]) } });
    renderApp("/repos/main/acme/payments?tab=settings");
    const box = await card();
    await userEvent.click(await within(box).findByRole("button", { name: "Remove reviewer 2" }));
    expect(fake.find("PUT", reviewersPath)[0].body).toEqual({
      reviewers: [[ref(entrySonnet)], [ref(entryConfig), ref(entryOpus)]],
      judge: [ref(entryOpus)],
    });
    expect(await within(box).findByText("Saved.")).toBeInTheDocument();
    expect(within(box).queryByRole("group", { name: "Reviewer 4" })).not.toBeInTheDocument();
  });

  it("removes the last extra reviewer and the judge with it", async () => {
    const fake = fakeApi({ ...routes(two), [`PUT ${reviewersPath}`]: { body: reviewersNone } });
    renderApp("/repos/main/acme/payments?tab=settings");
    const box = await card();
    await userEvent.click(await within(box).findByRole("button", { name: "Remove reviewer 2" }));
    expect(fake.find("PUT", reviewersPath)[0].body).toEqual({ reviewers: [], judge: [] });
    expect(await within(box).findByText("Saved.")).toBeInTheDocument();
    expect(within(box).queryByRole("group", { name: "Judge" })).not.toBeInTheDocument();
  });

  it("edits reviewer 2 and sends the judge as it is", async () => {
    const fake = fakeApi({ ...routes(two), [`PUT ${reviewersPath}`]: { body: two } });
    renderApp("/repos/main/acme/payments?tab=settings");
    const box = await card();
    await userEvent.click(await within(box).findByRole("button", { name: "Edit reviewer 2" }));
    const r2 = await group(box, "Reviewer 2");
    await userEvent.selectOptions(within(r2).getByRole("combobox", { name: "Entry to add" }), `global:${entrySonnet.id}`);
    await userEvent.click(within(r2).getByRole("button", { name: "Add" }));
    await userEvent.click(within(r2).getByRole("button", { name: "Save chain" }));
    expect(fake.find("PUT", reviewersPath)[0].body).toEqual({
      reviewers: [[ref(entryOpus), ref(entrySonnet)]],
      judge: [ref(entryOpus)],
    });
  });

  it("edits the judge and sends the reviewers as they are", async () => {
    const fake = fakeApi({ ...routes(two), [`PUT ${reviewersPath}`]: { body: two } });
    renderApp("/repos/main/acme/payments?tab=settings");
    const box = await card();
    await userEvent.click(await within(box).findByRole("button", { name: "Edit the judge" }));
    const judge = await group(box, "Judge");
    await userEvent.click(within(judge).getByRole("button", { name: `Remove ${entryOpus.name}` }));
    await userEvent.selectOptions(within(judge).getByRole("combobox", { name: "Entry to add" }), `team:${entryTeamQwen.id}`);
    await userEvent.click(within(judge).getByRole("button", { name: "Add" }));
    await userEvent.click(within(judge).getByRole("button", { name: "Save chain" }));
    expect(fake.find("PUT", reviewersPath)[0].body).toEqual({ reviewers: [[ref(entryOpus)]], judge: [ref(entryTeamQwen)] });
  });

  it("shows the server's message when a save is refused", async () => {
    fakeApi({
      ...routes(two),
      [`PUT ${reviewersPath}`]: { status: 400, body: { error: "reviewer 3 starts with the same model as reviewer 2" } },
    });
    renderApp("/repos/main/acme/payments?tab=settings");
    const box = await card();
    await userEvent.click(await within(box).findByRole("button", { name: "Add a reviewer" }));
    const r3 = await group(box, "Reviewer 3");
    await userEvent.selectOptions(within(r3).getByRole("combobox", { name: "Entry to add" }), `global:${entryOpus.id}`);
    await userEvent.click(within(r3).getByRole("button", { name: "Add" }));
    await userEvent.click(within(r3).getByRole("button", { name: "Save chain" }));
    expect(await within(box).findByRole("alert")).toHaveTextContent("reviewer 3 starts with the same model as reviewer 2");
  });

  it("leaves a viewer the reviewers to read, with no Edit, Remove or Add", async () => {
    fakeApi({
      ...routes(
        {
          ...four,
          readOnly: true,
          reviewers: four.reviewers.map((r) => ({ ...r, chain: { ...r.chain, readOnly: true, choices: [] } })),
          judge: { ...four.judge!, readOnly: true, choices: [] },
        },
        viewer,
      ),
      [`GET ${payments}/chain`]: { body: { ...chainRepoInherit, readOnly: true, choices: [] } },
    });
    renderApp("/repos/main/acme/payments?tab=inputs");
    const box = await card();
    expect(await group(box, "Reviewer 4")).toBeInTheDocument();
    expect(await group(box, "Judge")).toBeInTheDocument();
    expect(within(box).queryByRole("button", { name: /^Edit/ })).not.toBeInTheDocument();
    expect(within(box).queryByRole("button", { name: /^Remove/ })).not.toBeInTheDocument();
    expect(within(box).queryByRole("button", { name: "Add a reviewer" })).not.toBeInTheDocument();
    expect(box).toHaveTextContent("Each review now runs 4 reviews and 1 judge, about 4 to 5 times the cost of one review.");
  });

  it("shows the server's warnings, such as no judge", async () => {
    fakeApi(
      routes({
        ...reviewersWith([[entryOpus]], []),
        warnings: ["No judge: findings are merged without checking"],
      }),
    );
    renderApp("/repos/main/acme/payments?tab=settings");
    const box = await card();
    expect(await within(box).findByText("No judge: findings are merged without checking")).toBeInTheDocument();
    expect(within(await group(box, "Judge")).getByText("No entries.")).toBeInTheDocument();
  });

  it("disables Add with the server's refusal as its title", async () => {
    // With 1 reviewer and an admin, Add is enabled unless the server refuses.
    fakeApi(routes({ ...reviewersNone, addRefusal: "no review image supports multi review yet" }));
    renderApp("/repos/main/acme/payments?tab=settings");
    const add = await within(await card()).findByRole("button", { name: "Add a reviewer" });
    expect(add).toBeDisabled();
    expect(add).toHaveAttribute("title", "no review image supports multi review yet");
  });

  it("enables Add when the server does not refuse", async () => {
    fakeApi(routes());
    renderApp("/repos/main/acme/payments?tab=settings");
    const add = await within(await card()).findByRole("button", { name: "Add a reviewer" });
    await vi.waitFor(() => expect(add).toBeEnabled());
    expect(add).not.toHaveAttribute("title");
  });
});
