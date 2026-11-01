import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { fakeApi } from "../test/fakeApi";
import { librarySkills, platformAdmin, skillInstalled, skillWritten, viewer } from "../test/fixtures";
import { renderApp } from "../test/render";

afterEach(() => vi.unstubAllGlobals());

const rowOf = (name: string) => screen.getByRole("link", { name }).closest("[data-skill]") as HTMLElement;

describe("Skills", () => {
  it("lists name, version, source and the default badge", async () => {
    fakeApi({ "GET /api/me": { body: viewer }, "GET /api/skills": { body: librarySkills } });
    renderApp("/skills");
    expect(await screen.findByRole("heading", { name: "Skills" })).toBeInTheDocument();
    await screen.findByRole("link", { name: "code-review" });
    const written = rowOf("code-review");
    expect(written).toHaveTextContent("v2");
    expect(written).toHaveTextContent("written in the dashboard");
    expect(within(written).getByText("default")).toHaveClass("pill");
    const installed = rowOf("security-review");
    expect(installed).toHaveTextContent("v1");
    expect(installed).toHaveTextContent("acme/skills");
    expect(installed).toHaveTextContent("v1.0");
    expect(within(installed).queryByText("default")).not.toBeInTheDocument();
  });

  it("an admin sees New skill, Install from git, Edit, Copy and Delete; a viewer sees none", async () => {
    fakeApi({ "GET /api/me": { body: platformAdmin }, "GET /api/skills": { body: librarySkills } });
    const { unmount } = renderApp("/skills");
    await screen.findByRole("link", { name: "code-review" });
    expect(screen.getByRole("link", { name: "New skill" })).toHaveAttribute("href", "/skills/new");
    expect(screen.getByRole("link", { name: "Install from git" })).toHaveAttribute("href", "/skills/install");
    const written = rowOf("code-review");
    expect(within(written).getByRole("link", { name: "Edit" })).toHaveAttribute("href", "/skills/view/code-review");
    expect(within(written).getByRole("button", { name: "Copy" })).toBeInTheDocument();
    expect(within(written).getByRole("button", { name: "Delete" })).toBeInTheDocument();
    unmount();

    fakeApi({ "GET /api/me": { body: viewer }, "GET /api/skills": { body: librarySkills } });
    renderApp("/skills");
    await screen.findByRole("link", { name: "code-review" });
    expect(screen.queryByRole("link", { name: "New skill" })).not.toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "Install from git" })).not.toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "Edit" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Copy" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Delete" })).not.toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "Check for updates" })).not.toBeInTheDocument();
  });

  it("an installed skill has Copy and Check for updates, and no Edit", async () => {
    fakeApi({ "GET /api/me": { body: platformAdmin }, "GET /api/skills": { body: librarySkills } });
    renderApp("/skills");
    await screen.findByRole("link", { name: "security-review" });
    const installed = rowOf("security-review");
    expect(within(installed).getByRole("button", { name: "Copy" })).toBeInTheDocument();
    expect(within(installed).getByRole("link", { name: "Check for updates" })).toHaveAttribute("href", "/skills/updates?skill=security-review");
    expect(within(installed).queryByRole("link", { name: "Edit" })).not.toBeInTheDocument();
    const written = rowOf("code-review");
    expect(within(written).queryByRole("link", { name: "Check for updates" })).not.toBeInTheDocument();
  });

  it("copies a skill under a new name, with the CSRF token", async () => {
    let listed = 0;
    const fake = fakeApi({
      "GET /api/me": { body: platformAdmin },
      "GET /api/skills": () => ({
        body: listed++ === 0 ? librarySkills : [...librarySkills, { ...skillWritten, name: "my-review", source: null }],
      }),
      "POST /api/admin/skills/security-review/copy": { status: 201, body: { version: 1 } },
    });
    renderApp("/skills");
    await screen.findByRole("link", { name: "security-review" });
    await userEvent.click(within(rowOf("security-review")).getByRole("button", { name: "Copy" }));
    await userEvent.type(screen.getByLabelText("New name"), "my-review");
    await userEvent.click(screen.getByRole("button", { name: "Copy skill" }));
    const [post] = fake.find("POST", "/api/admin/skills/security-review/copy");
    expect(post.body).toEqual({ name: "my-review" });
    expect(post.headers.get("X-CSRF-Token")).toBe("csrf-123");
    expect(await screen.findByRole("link", { name: "my-review" })).toBeInTheDocument();
  });

  it("a taken copy name shows the server's message", async () => {
    fakeApi({
      "GET /api/me": { body: platformAdmin },
      "GET /api/skills": { body: librarySkills },
      "POST /api/admin/skills/code-review/copy": { status: 409, body: { error: "name already in the library" } },
    });
    renderApp("/skills");
    await screen.findByRole("link", { name: "code-review" });
    await userEvent.click(within(rowOf("code-review")).getByRole("button", { name: "Copy" }));
    await userEvent.type(screen.getByLabelText("New name"), "security-review");
    await userEvent.click(screen.getByRole("button", { name: "Copy skill" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("name already in the library");
  });

  it("deletes a skill after a confirmation", async () => {
    vi.stubGlobal("confirm", vi.fn(() => true));
    let listed = 0;
    const fake = fakeApi({
      "GET /api/me": { body: platformAdmin },
      "GET /api/skills": () => ({ body: listed++ === 0 ? librarySkills : [skillWritten] }),
      "DELETE /api/admin/skills/security-review": { status: 204 },
    });
    renderApp("/skills");
    await screen.findByRole("link", { name: "security-review" });
    await userEvent.click(within(rowOf("security-review")).getByRole("button", { name: "Delete" }));
    expect(fake.find("DELETE", "/api/admin/skills/security-review")[0].headers.get("X-CSRF-Token")).toBe("csrf-123");
    await vi.waitFor(() => expect(screen.queryByRole("link", { name: "security-review" })).not.toBeInTheDocument());
  });

  it("a delete 409 lists the repositories, the default list and '+2 repositories you can't see'", async () => {
    vi.stubGlobal("confirm", vi.fn(() => true));
    fakeApi({
      "GET /api/me": { body: platformAdmin },
      "GET /api/skills": { body: librarySkills },
      "DELETE /api/admin/skills/code-review": {
        status: 409,
        body: {
          error: "the skill is in use: take it off the repositories and the default list that name it first",
          repositories: [
            { instance: "main", project: "acme/widgets" },
            { instance: "main", project: "acme/payments" },
          ],
          hidden: 2,
          inDefaults: true,
        },
      },
    });
    renderApp("/skills");
    await screen.findByRole("link", { name: "code-review" });
    await userEvent.click(within(rowOf("code-review")).getByRole("button", { name: "Delete" }));
    const box = await screen.findByRole("alert");
    expect(box).toHaveTextContent("code-review is in use");
    expect(within(box).getByText("main: acme/widgets")).toBeInTheDocument();
    expect(within(box).getByText("main: acme/payments")).toBeInTheDocument();
    expect(within(box).getByText("the default skills")).toBeInTheDocument();
    expect(within(box).getByText("+2 repositories you can't see")).toBeInTheDocument();
  });

  it("a delete 409 with one hidden repository says '+1 repositories you can't see'", async () => {
    vi.stubGlobal("confirm", vi.fn(() => true));
    fakeApi({
      "GET /api/me": { body: platformAdmin },
      "GET /api/skills": { body: librarySkills },
      "DELETE /api/admin/skills/code-review": {
        status: 409,
        body: { error: "the skill is in use", repositories: [], hidden: 1, inDefaults: false },
      },
    });
    renderApp("/skills");
    await screen.findByRole("link", { name: "code-review" });
    await userEvent.click(within(rowOf("code-review")).getByRole("button", { name: "Delete" }));
    expect(await screen.findByText("+1 repositories you can't see")).toBeInTheDocument();
  });

  it("a hidden source shows 'a private repository on main'", async () => {
    const hidden = { ...skillInstalled, source: { kind: "instance" as const, instance: "main", ref: "v1.0", commit: "abc", hidden: true } };
    fakeApi({ "GET /api/me": { body: viewer }, "GET /api/skills": { body: [hidden] } });
    renderApp("/skills");
    await screen.findByRole("link", { name: "security-review" });
    expect(screen.getByText("a private repository on main")).toBeInTheDocument();
    expect(screen.queryByText(/acme\/skills/)).not.toBeInTheDocument();
  });

  it("shows a skill that came from a URL by its address, as text", async () => {
    const fromURL = { ...skillInstalled, source: { kind: "url" as const, url: "https://example.com/skills.git", ref: "main", commit: "abc" } };
    fakeApi({ "GET /api/me": { body: viewer }, "GET /api/skills": { body: [fromURL] } });
    renderApp("/skills");
    await screen.findByRole("link", { name: "security-review" });
    expect(screen.getByText("https://example.com/skills.git")).toBeInTheDocument();
  });

  it("says when the library has no skills", async () => {
    fakeApi({ "GET /api/me": { body: viewer }, "GET /api/skills": { body: [] } });
    renderApp("/skills");
    expect(await screen.findByText("The library has no skills.")).toBeInTheDocument();
  });
});
