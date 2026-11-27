import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import type { ChainView, SecuritySettingsView } from "../api";
import { fakeApi } from "../test/fakeApi";
import { chainRepoCustom, chainRepoInherit, librarySkills, repoDetail, repoPayments, repoTeams, securitySettings, ssoAdmin } from "../test/fixtures";
import { renderApp } from "../test/render";

afterEach(() => vi.unstubAllGlobals());

const payments = "/api/repositories/main/acme/payments";
const open = "/repos/main/acme/payments?tab=settings";
const inherited: ChainView = { ...chainRepoInherit, source: "chain", team: "" };

const noInputs = { branch: "main", skills: [], detected: [], instructions: "" };

const routes = (set: SecuritySettingsView = securitySettings, chain: ChainView = inherited, detail = repoDetail(repoPayments)) => ({
  "GET /api/skills": { body: librarySkills },
  [`GET ${payments}/inputs`]: { body: noInputs },
  [`GET ${payments}/teams`]: { body: repoTeams },
  "GET /api/me": { body: ssoAdmin },
  [`GET ${payments}`]: { body: detail },
  [`GET ${payments}/chain`]: { body: chainRepoInherit },
  [`GET ${payments}/reviewers`]: { body: { reviewers: [], judge: null, warnings: [], addRefusal: "" } },
  [`GET ${payments}/security/settings`]: { body: set },
  [`GET ${payments}/security/chain`]: { body: chain },
});

const card = () => screen.findByRole("region", { name: "Security scan" });

describe("SecurityScanSettings", () => {
  it("shows the defaults when nothing is set", async () => {
    fakeApi(routes());
    renderApp(open, { unfolded: true });
    const box = await card();
    expect(await within(box).findByText("The repository's model chain")).toBeInTheDocument();
    expect(within(box).getByLabelText("Max steps, main agent")).toHaveValue("");
    expect(within(box).getByLabelText("Max steps, main agent")).toHaveAttribute("placeholder", "from the skill (600)");
    expect(within(box).getByLabelText("Max steps, each sub-agent")).toHaveAttribute("placeholder", "from the skill (100)");
    expect(within(box).getByLabelText("Scan timeout (minutes)")).toHaveAttribute("placeholder", "3 h");
    expect(within(box).getByRole("switch", { name: "Use security findings in reviews" })).not.toBeChecked();
    expect(box).toHaveTextContent("They never block a merge.");
  });

  it("refuses a value out of range before saving", async () => {
    const fake = fakeApi(routes());
    renderApp(open, { unfolded: true });
    const box = await card();
    const main = await within(box).findByLabelText("Max steps, main agent");
    await userEvent.type(main, "4");
    await userEvent.click(within(box).getByRole("button", { name: "Save scan settings" }));
    expect(within(box).getByText("Max steps for the main agent: a whole number from 5 to 1000, or empty.")).toBeInTheDocument();
    expect(fake.find("PUT", `${payments}/security/settings`)).toHaveLength(0);
    await userEvent.clear(main);
    await userEvent.type(within(box).getByLabelText("Scan timeout (minutes)"), "5");
    await userEvent.click(within(box).getByRole("button", { name: "Save scan settings" }));
    expect(within(box).getByText("Scan timeout: a whole number of minutes from 10 to 360, or empty.")).toBeInTheDocument();
    expect(fake.find("PUT", `${payments}/security/settings`)).toHaveLength(0);
  });

  it("Save sends the 4 fields with null for empty, and keeps the others", async () => {
    const set = { ...securitySettings, maxSteps: 300, subagentMaxSteps: 50, findingsInReviews: true };
    const fake = fakeApi({ ...routes(set), [`PUT ${payments}/security/settings`]: { body: { ...set, timeoutSeconds: 1800 } } });
    renderApp(open, { unfolded: true });
    const box = await card();
    await userEvent.type(await within(box).findByLabelText("Scan timeout (minutes)"), "30");
    await userEvent.click(within(box).getByRole("button", { name: "Save scan settings" }));
    await vi.waitFor(() => expect(fake.find("PUT", `${payments}/security/settings`)).toHaveLength(1));
    expect(await within(box).findByText("Saved.")).toBeInTheDocument();
    expect(fake.find("PUT", `${payments}/security/settings`)[0].body).toEqual({
      maxSteps: 300,
      subagentMaxSteps: 50,
      timeoutSeconds: 1800,
      findingsInReviews: true,
    });
    await userEvent.clear(within(box).getByLabelText("Max steps, main agent"));
    await userEvent.click(within(box).getByRole("button", { name: "Save scan settings" }));
    await vi.waitFor(() => expect(fake.find("PUT", `${payments}/security/settings`)).toHaveLength(2));
    const puts = fake.find("PUT", `${payments}/security/settings`);
    expect(puts[puts.length - 1].body).toEqual({ maxSteps: null, subagentMaxSteps: 50, timeoutSeconds: 1800, findingsInReviews: true });
  });

  it("shows the server's 400 as it is", async () => {
    fakeApi({ ...routes(), [`PUT ${payments}/security/settings`]: { status: 400, body: { error: "max steps: out of range" } } });
    renderApp(open, { unfolded: true });
    const box = await card();
    await userEvent.click(await within(box).findByRole("switch", { name: "Use security findings in reviews" }));
    await userEvent.click(within(box).getByRole("button", { name: "Save scan settings" }));
    expect(await within(box).findByText("max steps: out of range")).toBeInTheDocument();
  });

  it("the chain edits on the security chain path and Inherit resets it", async () => {
    const fake = fakeApi({
      ...routes(),
      [`PUT ${payments}/security/chain`]: { body: { ...chainRepoCustom, entries: chainRepoCustom.entries } },
      [`POST ${payments}/security/chain/reset`]: { body: inherited },
    });
    renderApp(open, { unfolded: true });
    const box = await card();
    await userEvent.click(await within(box).findByRole("button", { name: "Edit scan model" }));
    await userEvent.click(within(box).getByRole("button", { name: "Save chain" }));
    await vi.waitFor(() => expect(fake.find("PUT", `${payments}/security/chain`)).toHaveLength(1));
    expect(fake.find("PUT", `${payments}/security/chain`)[0].body).toHaveProperty("entries");
    expect(await within(box).findByText("This repository has its own scan chain.")).toBeInTheDocument();
    vi.stubGlobal("confirm", () => true);
    await userEvent.click(within(box).getByRole("button", { name: "Inherit the repository's chain" }));
    await vi.waitFor(() => expect(fake.find("POST", `${payments}/security/chain/reset`)).toHaveLength(1));
  });

  it("a viewer sees values and no Edit or Save", async () => {
    const set = { ...securitySettings, maxSteps: 300, canEdit: false };
    fakeApi({ ...routes(set, { ...inherited, readOnly: true }) });
    renderApp(open, { unfolded: true });
    const box = await card();
    expect(await within(box).findByText("300")).toBeInTheDocument();
    expect(within(box).queryByRole("button", { name: "Save scan settings" })).not.toBeInTheDocument();
    expect(within(box).queryByRole("button", { name: "Edit scan model" })).not.toBeInTheDocument();
    expect(within(box).queryByLabelText("Max steps, main agent")).not.toBeInTheDocument();
    expect(within(box).queryByRole("alert")).not.toBeInTheDocument();
  });

  it("a maintainer who is not of the owner team edits the limits but not the chain", async () => {
    fakeApi(routes(securitySettings, { ...inherited, readOnly: true }));
    renderApp(open, { unfolded: true });
    const box = await card();
    expect(await within(box).findByRole("button", { name: "Save scan settings" })).toBeInTheDocument();
    expect(within(box).queryByRole("button", { name: "Edit scan model" })).not.toBeInTheDocument();
    expect(within(box).queryByRole("button", { name: "Inherit the repository's chain" })).not.toBeInTheDocument();
  });

  it("a paused repository shows values, a note and no edit controls", async () => {
    const paused = repoDetail({ ...repoPayments, paused: true, on: false, health: "paused" });
    fakeApi(routes(securitySettings, chainRepoCustom, paused));
    renderApp(open, { unfolded: true });
    const box = await card();
    expect(await within(box).findByText(/This repository is paused/)).toBeInTheDocument();
    expect(within(box).queryByRole("button", { name: "Save scan settings" })).not.toBeInTheDocument();
    expect(within(box).queryByRole("button", { name: "Edit scan model" })).not.toBeInTheDocument();
    expect(within(box).queryByRole("button", { name: "Inherit the repository's chain" })).not.toBeInTheDocument();
    expect(within(box).queryByLabelText("Max steps, main agent")).not.toBeInTheDocument();
    expect(within(box).queryByRole("alert")).not.toBeInTheDocument();
  });
});
