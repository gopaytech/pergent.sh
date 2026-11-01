import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { fakeApi } from "../test/fakeApi";
import { platformAdmin, skillInstalledDetail, skillWrittenDetail, viewer } from "../test/fixtures";
import { renderApp } from "../test/render";

afterEach(() => vi.unstubAllGlobals());

describe("SkillEditor: new", () => {
  it("create posts name, content and addToDefaults; Add to default skills is off at first", async () => {
    const fake = fakeApi({
      "GET /api/me": { body: platformAdmin },
      "GET /api/skills": { body: [] },
      "POST /api/admin/skills": { status: 201, body: { version: 1 } },
    });
    renderApp("/skills/new");
    const box = await screen.findByLabelText("Add to default skills");
    expect(box).not.toBeChecked();
    await userEvent.type(screen.getByLabelText("Name"), "my-skill");
    await userEvent.type(screen.getByLabelText("Skill text"), "Review {{the code.");
    await userEvent.click(screen.getByRole("button", { name: "Create skill" }));
    const [post] = fake.find("POST", "/api/admin/skills");
    expect(post.body).toEqual({ name: "my-skill", content: "Review {the code.", addToDefaults: false });
    expect(post.headers.get("X-CSRF-Token")).toBe("csrf-123");
    expect(await screen.findByTestId("location")).toHaveTextContent("/skills");
  });

  it("sends addToDefaults when the box is ticked", async () => {
    const fake = fakeApi({
      "GET /api/me": { body: platformAdmin },
      "GET /api/skills": { body: [] },
      "POST /api/admin/skills": { status: 201, body: { version: 1 } },
    });
    renderApp("/skills/new");
    await userEvent.type(await screen.findByLabelText("Name"), "my-skill");
    await userEvent.type(screen.getByLabelText("Skill text"), "text");
    await userEvent.click(screen.getByLabelText("Add to default skills"));
    await userEvent.click(screen.getByRole("button", { name: "Create skill" }));
    expect(fake.find("POST", "/api/admin/skills")[0].body).toMatchObject({ addToDefaults: true });
  });

  it("shows the server's refusal and stays on the form", async () => {
    fakeApi({
      "GET /api/me": { body: platformAdmin },
      "POST /api/admin/skills": { status: 409, body: { error: "name already in the library" } },
    });
    renderApp("/skills/new");
    await userEvent.type(await screen.findByLabelText("Name"), "code-review");
    await userEvent.type(screen.getByLabelText("Skill text"), "text");
    await userEvent.click(screen.getByRole("button", { name: "Create skill" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("name already in the library");
    expect(screen.getByLabelText("Name")).toHaveValue("code-review");
  });

  it("is not there for a viewer", async () => {
    fakeApi({ "GET /api/me": { body: viewer } });
    renderApp("/skills/new");
    expect(await screen.findByRole("heading", { name: "Not found" })).toBeInTheDocument();
  });
});

describe("SkillEditor: one skill", () => {
  it("renders skill content as text", async () => {
    const detail = { ...skillWrittenDetail, content: "<script>x</script>\n# h\n" };
    fakeApi({ "GET /api/me": { body: viewer }, "GET /api/skills/code-review": { body: detail } });
    const { container } = renderApp("/skills/code-review");
    const pre = await screen.findByText(/<script>x<\/script>/);
    expect(pre.tagName).toBe("PRE");
    expect(pre).toHaveTextContent("# h");
    expect(container.querySelector("script")).toBeNull();
    expect(screen.queryByRole("heading", { name: "h" })).not.toBeInTheDocument();
    expect(screen.queryByRole("textbox")).not.toBeInTheDocument();
  });

  it("an admin edits a written skill and saves with the version it opened", async () => {
    const fake = fakeApi({
      "GET /api/me": { body: platformAdmin },
      "GET /api/skills/code-review": { body: skillWrittenDetail },
      "PUT /api/admin/skills/code-review": { body: { version: 3 } },
    });
    renderApp("/skills/code-review");
    const text = await screen.findByLabelText("Skill text");
    expect(text).toHaveValue(skillWrittenDetail.content);
    await userEvent.type(text, "More.");
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    const [put] = fake.find("PUT", "/api/admin/skills/code-review");
    expect(put.body).toEqual({ content: skillWrittenDetail.content + "More.", version: 2 });
    expect(put.headers.get("X-CSRF-Token")).toBe("csrf-123");
    expect(await screen.findByText("Saved as v3.")).toBeInTheDocument();
  });

  it("a stale save shows 'changed since you opened it' with Reload", async () => {
    let reads = 0;
    fakeApi({
      "GET /api/me": { body: platformAdmin },
      "GET /api/skills/code-review": () => ({
        body: reads++ === 0 ? skillWrittenDetail : { ...skillWrittenDetail, version: 3, content: "Newer text.\n" },
      }),
      "PUT /api/admin/skills/code-review": { status: 409, body: { error: "this skill changed since you opened it" } },
    });
    renderApp("/skills/code-review");
    await userEvent.type(await screen.findByLabelText("Skill text"), "mine");
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("this skill changed since you opened it");
    await userEvent.click(screen.getByRole("button", { name: "Reload" }));
    expect(await screen.findByLabelText("Skill text")).toHaveValue("Newer text.\n");
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  });

  it("an installed skill is read-only for an admin, who can copy it", async () => {
    fakeApi({ "GET /api/me": { body: platformAdmin }, "GET /api/skills/security-review": { body: skillInstalledDetail } });
    renderApp("/skills/security-review");
    const pre = await screen.findByText(/Look for security bugs\./);
    expect(pre.tagName).toBe("PRE");
    expect(screen.queryByRole("textbox")).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Save" })).not.toBeInTheDocument();
    expect(screen.getByText(/can't be edited/)).toBeInTheDocument();
    expect(screen.getByText(/acme\/skills/)).toBeInTheDocument();
  });

  it("lists the versions, and shows an old one as text", async () => {
    const old = { version: 1, at: "2026-10-31T08:00:00Z", actor: "admin", commit: "", content: "<b>old</b> text\n" };
    fakeApi({
      "GET /api/me": { body: viewer },
      "GET /api/skills/code-review": { body: skillWrittenDetail },
      "GET /api/skills/code-review/versions/1": { body: old },
    });
    renderApp("/skills/code-review");
    const history = await screen.findByRole("region", { name: "Versions" });
    expect(within(history).getAllByRole("button", { name: /^View v/ })).toHaveLength(2);
    await userEvent.click(within(history).getByRole("button", { name: "View v1" }));
    const pre = await screen.findByText(/<b>old<\/b> text/);
    expect(pre.tagName).toBe("PRE");
  });

  it("says so for a skill that is not in the library", async () => {
    fakeApi({ "GET /api/me": { body: viewer } });
    renderApp("/skills/nope");
    expect(await screen.findByRole("heading", { name: "Not found" })).toBeInTheDocument();
  });
});
