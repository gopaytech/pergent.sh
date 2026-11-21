import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import type { ChainView } from "../api";
import { fakeApi } from "../test/fakeApi";
import {
  chainRepoInherit,
  entryOpus,
  entrySonnet,
  librarySkills,
  panelRepoCustom,
  panelRepoInherit,
  panelRepoOff,
  repoDetail,
  repoPayments,
  repoTeams,
  ssoAdmin,
  viewer,
} from "../test/fixtures";
import { renderApp } from "../test/render";

afterEach(() => vi.unstubAllGlobals());

const payments = "/api/repositories/main/acme/payments";
const panelPath = `${payments}/panel-chain`;
const routes = (panel: ChainView = panelRepoInherit, me = ssoAdmin) => ({
  "GET /api/me": { body: me },
  [`GET ${payments}`]: { body: repoDetail(repoPayments, me === ssoAdmin ? "admin" : "viewer") },
  "GET /api/skills": { body: librarySkills },
  [`GET ${payments}/inputs`]: { body: { branch: "main", skills: [], detected: [], instructions: "" } },
  [`GET ${payments}/teams`]: { body: repoTeams },
  [`GET ${payments}/chain`]: { body: chainRepoInherit },
  [`GET ${panelPath}`]: { body: panel },
});
const card = () => screen.findByRole("region", { name: "Chain B (review panel)" });

describe("RepoModelChain: Chain B (review panel)", () => {
  it("is a section of its own beside the model chain, which stays as it is", async () => {
    fakeApi(routes());
    renderApp("/repos/main/acme/payments?tab=settings");
    expect(await screen.findByRole("region", { name: "Model chain" })).toBeInTheDocument();
    const box = await card();
    expect(await within(box).findByText("This repository has no chain B, so no review panel.")).toBeInTheDocument();
    expect(within(box).getByRole("button", { name: "Inherit" })).toHaveAttribute("aria-pressed", "true");
    expect(within(box).getByRole("button", { name: "Custom" })).toHaveAttribute("aria-pressed", "false");
    expect(within(box).queryByRole("button", { name: "Save chain" })).not.toBeInTheDocument();
  });

  it("turns the panel off with a POST and says so", async () => {
    const fake = fakeApi({ ...routes(), [`POST ${panelPath}/off`]: { body: panelRepoOff } });
    renderApp("/repos/main/acme/payments?tab=settings");
    const box = await card();
    await userEvent.click(await within(box).findByRole("button", { name: "Off" }));
    expect(await within(box).findByText("Panel: off for this repository")).toBeInTheDocument();
    expect(fake.find("POST", `${panelPath}/off`)).toHaveLength(1);
    expect(within(box).getByRole("button", { name: "Off" })).toHaveAttribute("aria-pressed", "true");
  });

  it("goes back to off from a started Custom with no request", async () => {
    const fake = fakeApi({ ...routes(panelRepoOff), [`POST ${panelPath}/off`]: { body: panelRepoOff } });
    renderApp("/repos/main/acme/payments?tab=settings");
    const box = await card();
    expect(await within(box).findByText("Panel: off for this repository")).toBeInTheDocument();
    await userEvent.click(within(box).getByRole("button", { name: "Custom" }));
    expect(within(box).getByText("Choose the entries of its own chain B.")).toBeInTheDocument();
    await userEvent.click(within(box).getByRole("button", { name: "Off" }));
    expect(within(box).getByText("Panel: off for this repository")).toBeInTheDocument();
    expect(within(box).getByRole("button", { name: "Off" })).toHaveAttribute("aria-pressed", "true");
    expect(fake.find("POST", `${panelPath}/off`)).toHaveLength(0);
  });

  it("gives the repository its own chain B with a PUT of the entries", async () => {
    const fake = fakeApi({ ...routes(), [`PUT ${panelPath}`]: { body: panelRepoCustom } });
    renderApp("/repos/main/acme/payments?tab=settings");
    const box = await card();
    await userEvent.click(await within(box).findByRole("button", { name: "Custom" }));
    await userEvent.selectOptions(within(box).getByRole("combobox", { name: "Entry to add" }), `global:${entryOpus.id}`);
    await userEvent.click(within(box).getByRole("button", { name: "Add" }));
    await userEvent.click(within(box).getByRole("button", { name: "Save chain" }));
    expect(fake.find("PUT", panelPath)[0].body).toEqual({ entries: [{ source: "global", id: entryOpus.id }] });
    expect(await within(box).findByText("This repository has its own chain B.")).toBeInTheDocument();
  });

  it("shows the message of the image check when a save is refused", async () => {
    fakeApi({
      ...routes(),
      [`PUT ${panelPath}`]: { status: 400, body: { error: "no review image supports a review panel yet" } },
    });
    renderApp("/repos/main/acme/payments?tab=settings");
    const box = await card();
    await userEvent.click(await within(box).findByRole("button", { name: "Custom" }));
    await userEvent.selectOptions(within(box).getByRole("combobox", { name: "Entry to add" }), `global:${entrySonnet.id}`);
    await userEvent.click(within(box).getByRole("button", { name: "Add" }));
    await userEvent.click(within(box).getByRole("button", { name: "Save chain" }));
    expect(await within(box).findByText(/no review image supports a review panel yet/)).toBeInTheDocument();
  });

  it("goes back to inherit with a POST of reset, after a confirmation", async () => {
    const confirm = vi.fn(() => true);
    vi.stubGlobal("confirm", confirm);
    const fake = fakeApi({ ...routes(panelRepoCustom), [`POST ${panelPath}/reset`]: { body: panelRepoInherit } });
    renderApp("/repos/main/acme/payments?tab=settings");
    const box = await card();
    expect(await within(box).findByText("This repository has its own chain B.")).toBeInTheDocument();
    await userEvent.click(within(box).getByRole("button", { name: "Inherit" }));
    expect(confirm).toHaveBeenCalledOnce();
    expect(await within(box).findByText("This repository has no chain B, so no review panel.")).toBeInTheDocument();
    expect(fake.find("POST", `${panelPath}/reset`)).toHaveLength(1);
  });

  it("leaves a viewer a read-only chain B, with no mode buttons and no Save", async () => {
    fakeApi(routes({ ...panelRepoCustom, readOnly: true, choices: [] }, viewer));
    renderApp("/repos/main/acme/payments?tab=inputs");
    const box = await card();
    expect(await within(box).findByText(entryOpus.name)).toBeInTheDocument();
    expect(within(box).queryByRole("button", { name: "Inherit" })).not.toBeInTheDocument();
    expect(within(box).queryByRole("button", { name: "Save chain" })).not.toBeInTheDocument();
    expect(within(box).queryByRole("button", { name: /Move .* up/ })).not.toBeInTheDocument();
    expect(within(box).getByText(/Only admins and maintainers/)).toBeInTheDocument();
  });
});

describe("RepositoryDetail: the panel line", () => {
  const withPanel = (panel: { on: boolean; mode: "inherit" | "custom" | "off"; a?: string; b?: string }) => ({
    ...routes(),
    [`GET ${payments}`]: { body: { ...repoDetail(repoPayments), panel } },
  });

  it("names position 1 of both chains while the panel is on", async () => {
    fakeApi(withPanel({ on: true, mode: "inherit", a: "Claude Sonnet", b: "Claude Opus" }));
    renderApp("/repos/main/acme/payments");
    expect(await screen.findByText("Panel: on (A: Claude Sonnet, B: Claude Opus)")).toBeInTheDocument();
  });

  it("says the panel is off", async () => {
    fakeApi(withPanel({ on: false, mode: "off" }));
    renderApp("/repos/main/acme/payments");
    expect(await screen.findByText("Panel: off")).toBeInTheDocument();
  });
});
