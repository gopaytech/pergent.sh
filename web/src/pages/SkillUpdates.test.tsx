import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import type { SkillDetail } from "../api";
import { fakeApi } from "../test/fakeApi";
import { fetchUpdateDone, platformAdmin, skillInstalledDetail, skillWrittenDetail, viewer } from "../test/fixtures";
import { renderApp } from "../test/render";

afterEach(() => vi.unstubAllGlobals());

const id = fetchUpdateDone.id;
const fetchUrl = `/api/admin/skill-fetches/${id}`;
const skillUrl = "/api/skills/security-review";
const page = "/skills/updates?skill=security-review";

const withSource = (source: Partial<NonNullable<SkillDetail["source"]>>): SkillDetail => ({
  ...skillInstalledDetail,
  source: { ...skillInstalledDetail.source!, ...source },
});

const rowOf = (name: string) => screen.getByRole("heading", { name }).closest("[data-update]") as HTMLElement;

async function checkAgainst(ref: string) {
  const box = await screen.findByLabelText("Branch or tag");
  await userEvent.clear(box);
  await userEvent.type(box, ref);
  await userEvent.click(screen.getByRole("button", { name: "Check for updates" }));
}

describe("SkillUpdates", () => {
  it("starts from the skill's source: its folder, repository and ref", async () => {
    fakeApi({
      "GET /api/me": { body: platformAdmin },
      [`GET ${skillUrl}`]: { body: skillInstalledDetail },
    });
    renderApp(page);
    expect(await screen.findByLabelText("Repository")).toHaveValue("acme/skills");
    expect(screen.getByLabelText("Folder")).toHaveValue("skills");
    expect(screen.getByLabelText("Branch or tag")).toHaveValue("v1.0");
    expect(screen.getByText(/is installed from/)).toHaveTextContent("security-review is installed from acme/skills on main");
  });

  it("a new tag is sent as ref, with fromRef, the repository and the folder", async () => {
    const fake = fakeApi({
      "GET /api/me": { body: platformAdmin },
      [`GET ${skillUrl}`]: { body: skillInstalledDetail },
      "POST /api/admin/skill-fetches": { status: 202, body: { id } },
      [`GET ${fetchUrl}`]: { body: fetchUpdateDone },
    });
    renderApp(page);
    await checkAgainst("v1.1");
    expect(fake.find("POST", "/api/admin/skill-fetches")[0].body).toEqual({
      instance: "main",
      repo: "acme/skills",
      ref: "v1.1",
      path: "skills",
      purpose: "update",
      fromRef: "v1.0",
    });
    await screen.findByRole("region", { name: "Updates found" });
  });

  it("a public URL source is sent as url", async () => {
    const fake = fakeApi({
      "GET /api/me": { body: platformAdmin },
      [`GET ${skillUrl}`]: {
        body: withSource({
          kind: "url",
          instance: undefined,
          repo: undefined,
          url: "https://example.com/skills.git",
          path: "pack/security-review/SKILL.md",
        }),
      },
      "POST /api/admin/skill-fetches": { status: 202, body: { id } },
      [`GET ${fetchUrl}`]: { body: fetchUpdateDone },
    });
    renderApp(page);
    expect(await screen.findByLabelText("Folder")).toHaveValue("pack");
    await userEvent.click(screen.getByRole("button", { name: "Check for updates" }));
    expect(fake.find("POST", "/api/admin/skill-fetches")[0].body).toEqual({
      url: "https://example.com/skills.git",
      ref: "v1.0",
      path: "pack",
      purpose: "update",
      fromRef: "v1.0",
    });
  });

  it("a skill at the top of its repository checks the top folder (no path sent)", async () => {
    const fake = fakeApi({
      "GET /api/me": { body: platformAdmin },
      [`GET ${skillUrl}`]: { body: withSource({ path: "security-review.md" }) },
      "POST /api/admin/skill-fetches": { status: 202, body: { id } },
      [`GET ${fetchUrl}`]: { body: fetchUpdateDone },
    });
    renderApp(page);
    expect(await screen.findByLabelText("Folder")).toHaveValue("");
    await userEvent.click(screen.getByRole("button", { name: "Check for updates" }));
    expect(fake.find("POST", "/api/admin/skill-fetches")[0].body).not.toHaveProperty("path");
  });

  it("a private repository the admin can't see is typed in", async () => {
    fakeApi({
      "GET /api/me": { body: platformAdmin },
      [`GET ${skillUrl}`]: {
        body: withSource({ repo: undefined, path: undefined, hidden: true }),
      },
    });
    renderApp(page);
    expect(await screen.findByLabelText("Repository")).toHaveValue("");
    expect(screen.getByText(/Type the repository to check it/)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Check for updates" })).toBeDisabled();
  });

  it("shows a diff per skill, as text", async () => {
    const hostile = {
      ...fetchUpdateDone,
      updates: [
        {
          ...fetchUpdateDone.updates[0],
          fetched: "Look for bugs.\n<img src=x onerror=alert(1)>\n",
        },
        ...fetchUpdateDone.updates.slice(1),
      ],
    };
    fakeApi({
      "GET /api/me": { body: platformAdmin },
      [`GET ${skillUrl}`]: { body: skillInstalledDetail },
      "POST /api/admin/skill-fetches": { status: 202, body: { id } },
      [`GET ${fetchUrl}`]: { body: hostile },
    });
    const { container } = renderApp(page);
    await checkAgainst("v1.1");
    await screen.findByRole("region", { name: "Updates found" });
    const changed = rowOf("security-review");
    const diff = within(changed).getByRole("group", { name: "Changes" });
    expect(diff.tagName).toBe("PRE");
    expect(diff).toHaveTextContent("- Check inputs.");
    expect(diff).toHaveTextContent("+ <img src=x onerror=alert(1)>");
    expect(diff).toHaveTextContent("Look for bugs.");
    expect(container.querySelector("img")).toBeNull();
    expect(within(rowOf("style")).getByText("No change")).toBeInTheDocument();
  });

  it("shows 'removed at the source' and 'renamed at the source' with Update disabled", async () => {
    fakeApi({
      "GET /api/me": { body: platformAdmin },
      [`GET ${skillUrl}`]: { body: skillInstalledDetail },
      "POST /api/admin/skill-fetches": { status: 202, body: { id } },
      [`GET ${fetchUrl}`]: { body: fetchUpdateDone },
    });
    renderApp(page);
    await checkAgainst("v1.1");
    await screen.findByRole("region", { name: "Updates found" });
    const gone = rowOf("old");
    expect(gone).toHaveTextContent("removed at the source");
    expect(within(gone).getByRole("button", { name: "Update" })).toBeDisabled();
    const moved = rowOf("moved");
    expect(moved).toHaveTextContent("renamed at the source");
    expect(moved).toHaveTextContent(/install it again/i);
    expect(within(moved).getByRole("button", { name: "Update" })).toBeDisabled();
    expect(within(rowOf("security-review")).getByRole("button", { name: "Update" })).toBeEnabled();
  });

  it("Update posts the name and shows the result; the library views reload", async () => {
    const fake = fakeApi({
      "GET /api/me": { body: platformAdmin },
      [`GET ${skillUrl}`]: { body: skillInstalledDetail },
      "POST /api/admin/skill-fetches": { status: 202, body: { id } },
      [`GET ${fetchUrl}`]: { body: fetchUpdateDone },
      [`POST ${fetchUrl}/update`]: {
        body: [{ name: "security-review", ok: true }],
      },
    });
    renderApp(page);
    await checkAgainst("v1.1");
    await screen.findByRole("region", { name: "Updates found" });
    await userEvent.click(within(rowOf("security-review")).getByRole("button", { name: "Update" }));
    expect(fake.find("POST", `${fetchUrl}/update`)[0].body).toEqual({
      names: ["security-review"],
    });
    expect(await within(rowOf("security-review")).findByText("Updated")).toBeInTheDocument();
    expect(within(rowOf("security-review")).getByRole("button", { name: "Update" })).toBeDisabled();
  });

  it("shows the server's refusal beside the skill", async () => {
    fakeApi({
      "GET /api/me": { body: platformAdmin },
      [`GET ${skillUrl}`]: { body: skillInstalledDetail },
      "POST /api/admin/skill-fetches": { status: 202, body: { id } },
      [`GET ${fetchUrl}`]: { body: fetchUpdateDone },
      [`POST ${fetchUrl}/update`]: {
        body: [
          {
            name: "security-review",
            ok: false,
            error: "the skill contains a secret pergent.sh holds",
          },
        ],
      },
    });
    renderApp(page);
    await checkAgainst("v1.1");
    await screen.findByRole("region", { name: "Updates found" });
    await userEvent.click(within(rowOf("security-review")).getByRole("button", { name: "Update" }));
    expect(await screen.findByText("the skill contains a secret pergent.sh holds")).toBeInTheDocument();
  });

  it("says a skill written in the dashboard has no source", async () => {
    fakeApi({
      "GET /api/me": { body: platformAdmin },
      "GET /api/skills/code-review": { body: skillWrittenDetail },
    });
    renderApp("/skills/updates?skill=code-review");
    expect(await screen.findByText(/written in the dashboard/)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Check for updates" })).not.toBeInTheDocument();
  });

  it("asks for a skill when none is named", async () => {
    fakeApi({ "GET /api/me": { body: platformAdmin } });
    renderApp("/skills/updates");
    expect(await screen.findByText(/Choose a skill/)).toBeInTheDocument();
  });

  it("is not there for a viewer", async () => {
    fakeApi({ "GET /api/me": { body: viewer } });
    renderApp(page);
    expect(await screen.findByRole("heading", { name: "Not found" })).toBeInTheDocument();
  });
});
