import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { fakeApi, type Reply } from "../test/fakeApi";
import { fetchInstallDone, operator, platformAdmin, viewer } from "../test/fixtures";
import { renderApp } from "../test/render";

afterEach(() => vi.unstubAllGlobals());

const id = fetchInstallDone.id;
const fetchUrl = `/api/admin/skill-fetches/${id}`;

// readThen answers the first read as running and the rest as done.
function readThen(done = fetchInstallDone): (call: unknown) => Reply {
  let n = 0;
  return () => ({
    body: n++ === 0 ? { ...done, status: "running", skills: [] } : done,
  });
}

async function readRepo(ref = "v1.0") {
  await userEvent.type(await screen.findByLabelText("Repository"), "acme/skills");
  await userEvent.type(screen.getByLabelText("Branch or tag"), ref);
  await userEvent.type(screen.getByLabelText("Folder"), "skills");
  await userEvent.click(screen.getByRole("button", { name: "Read skills" }));
}

describe("SkillInstall", () => {
  it("reads an instance repository with its ref and folder, polls until done, and lists the skills with size and problem", async () => {
    const fake = fakeApi({
      "GET /api/me": { body: platformAdmin },
      "POST /api/admin/skill-fetches": { status: 202, body: { id } },
      [`GET ${fetchUrl}`]: readThen(),
    });
    renderApp("/skills/install");
    expect(await screen.findByLabelText("Instance")).toHaveValue("main");
    await readRepo();
    const [post] = fake.find("POST", "/api/admin/skill-fetches");
    expect(post.body).toEqual({
      instance: "main",
      repo: "acme/skills",
      ref: "v1.0",
      path: "skills",
      purpose: "install",
    });
    expect(post.headers.get("X-CSRF-Token")).toBe("csrf-123");
    expect(await screen.findByText("Reading the source…")).toBeInTheDocument();
    const list = await screen.findByRole("region", { name: "Skills found" }, { timeout: 4000 });
    expect(fake.find("GET", fetchUrl).length).toBeGreaterThanOrEqual(2);
    expect(within(list).getByText("style")).toBeInTheDocument();
    expect(list).toHaveTextContent("2 KiB");
    expect(list).toHaveTextContent("larger than 128 KiB");
    expect(list).toHaveTextContent("commit 1234567");
    expect(within(list).getByRole("checkbox", { name: "style" })).toBeEnabled();
    expect(within(list).getByRole("checkbox", { name: "huge" })).toBeDisabled();
  });

  it("a public URL is sent without an instance or a repository", async () => {
    const fake = fakeApi({
      "GET /api/me": { body: operator },
      "POST /api/admin/skill-fetches": { status: 202, body: { id } },
      [`GET ${fetchUrl}`]: {
        body: {
          ...fetchInstallDone,
          instance: undefined,
          repo: undefined,
          url: "https://example.com/skills.git",
        },
      },
    });
    renderApp("/skills/install");
    await userEvent.click(await screen.findByRole("radio", { name: "A public URL" }));
    expect(screen.queryByLabelText("Repository")).not.toBeInTheDocument();
    await userEvent.type(screen.getByLabelText("URL"), "https://example.com/skills.git");
    await userEvent.type(screen.getByLabelText("Branch or tag"), "main");
    await userEvent.click(screen.getByRole("button", { name: "Read skills" }));
    expect(fake.find("POST", "/api/admin/skill-fetches")[0].body).toEqual({
      url: "https://example.com/skills.git",
      ref: "main",
      purpose: "install",
    });
    await screen.findByRole("region", { name: "Skills found" });
  });

  it("an operator types the instance; a platform admin's is its own", async () => {
    fakeApi({ "GET /api/me": { body: operator } });
    const { unmount } = renderApp("/skills/install");
    const box = await screen.findByLabelText("Instance");
    expect(box).toHaveValue("");
    expect(box).toBeEnabled();
    unmount();
    fakeApi({ "GET /api/me": { body: platformAdmin } });
    renderApp("/skills/install");
    expect(await screen.findByLabelText("Instance")).toBeDisabled();
  });

  it("the URL help says the Job reaches only jobEgress.cidrs", async () => {
    fakeApi({ "GET /api/me": { body: operator } });
    renderApp("/skills/install");
    await userEvent.click(await screen.findByRole("radio", { name: "A public URL" }));
    expect(screen.getByText(/reaches only jobEgress\.cidrs/)).toBeInTheDocument();
  });

  it("says installed skills are readable by every signed-in user", async () => {
    fakeApi({ "GET /api/me": { body: platformAdmin } });
    renderApp("/skills/install");
    expect(await screen.findByText(/readable by every signed-in user/)).toBeInTheDocument();
  });

  it("a taken name can't be ticked and says 'name already in the library'", async () => {
    fakeApi({
      "GET /api/me": { body: platformAdmin },
      "POST /api/admin/skill-fetches": { status: 202, body: { id } },
      [`GET ${fetchUrl}`]: { body: fetchInstallDone },
    });
    renderApp("/skills/install");
    await readRepo();
    const list = await screen.findByRole("region", { name: "Skills found" });
    expect(within(list).getByRole("checkbox", { name: "code-review" })).toBeDisabled();
    expect(list).toHaveTextContent("name already in the library");
  });

  it("installs the ticked names; Add to default skills is off at first and is sent when ticked", async () => {
    const fake = fakeApi({
      "GET /api/me": { body: platformAdmin },
      "GET /api/skills": { body: [] },
      "POST /api/admin/skill-fetches": { status: 202, body: { id } },
      [`GET ${fetchUrl}`]: { body: fetchInstallDone },
      [`POST ${fetchUrl}/install`]: { body: [{ name: "style", ok: true }] },
    });
    renderApp("/skills/install");
    await readRepo();
    const defaults = await screen.findByLabelText("Add to default skills");
    expect(defaults).not.toBeChecked();
    const install = screen.getByRole("button", { name: "Install" });
    expect(install).toBeDisabled();
    await userEvent.click(screen.getByRole("checkbox", { name: "style" }));
    await userEvent.click(defaults);
    await userEvent.click(install);
    expect(fake.find("POST", `${fetchUrl}/install`)[0].body).toEqual({
      names: ["style"],
      addToDefaults: true,
    });
    expect(await screen.findByText("Installed")).toBeInTheDocument();
  });

  it("shows each name's refusal beside it", async () => {
    fakeApi({
      "GET /api/me": { body: platformAdmin },
      "POST /api/admin/skill-fetches": { status: 202, body: { id } },
      [`GET ${fetchUrl}`]: { body: fetchInstallDone },
      [`POST ${fetchUrl}/install`]: {
        body: [
          {
            name: "style",
            ok: false,
            error: "the skill contains a secret pergent.sh holds",
          },
        ],
      },
    });
    renderApp("/skills/install");
    await readRepo();
    await userEvent.click(await screen.findByRole("checkbox", { name: "style" }));
    await userEvent.click(screen.getByRole("button", { name: "Install" }));
    expect(await screen.findByText("the skill contains a secret pergent.sh holds")).toBeInTheDocument();
  });

  it("shows the server's refusal to start a read (the ceiling, a missing image)", async () => {
    fakeApi({
      "GET /api/me": { body: platformAdmin },
      "POST /api/admin/skill-fetches": {
        status: 403,
        body: {
          error: "you can install only from repositories of your own instance that you can see",
        },
      },
    });
    renderApp("/skills/install");
    await readRepo();
    expect(await screen.findByRole("alert")).toHaveTextContent("you can install only from repositories of your own instance");
  });

  it("shows why a read failed and lets the admin read again", async () => {
    fakeApi({
      "GET /api/me": { body: platformAdmin },
      "POST /api/admin/skill-fetches": { status: 202, body: { id } },
      [`GET ${fetchUrl}`]: {
        body: {
          ...fetchInstallDone,
          status: "failed",
          error: "the branch or tag was not found",
          skills: [],
        },
      },
    });
    renderApp("/skills/install");
    await readRepo("nope");
    expect(await screen.findByText("the branch or tag was not found")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Read skills" })).toBeEnabled();
  });

  it("is not there for a viewer", async () => {
    fakeApi({ "GET /api/me": { body: viewer } });
    renderApp("/skills/install");
    expect(await screen.findByRole("heading", { name: "Not found" })).toBeInTheDocument();
  });
});
