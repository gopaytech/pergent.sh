import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import type { ChainView, ModelAdminView, ModelsAdminView } from "../api";
import { fakeApi } from "../test/fakeApi";
import {
  chainGlobal,
  choice,
  commentFooter,
  defaults,
  entryConfig,
  entryOpus,
  entrySonnet,
  librarySkills,
  modelOpus,
  modelSonnet,
  models,
  operator,
  ssoAdmin,
  viewer,
} from "../test/fixtures";
import { renderApp } from "../test/render";

afterEach(() => vi.unstubAllGlobals());

const routes = (me = ssoAdmin) => ({
  "GET /api/me": { body: me },
  "GET /api/admin/defaults": { body: defaults },
  "GET /api/admin/comment-footer": { body: commentFooter },
  "GET /api/skills": { body: librarySkills },
  "GET /api/admin/models": { body: models },
  "GET /api/admin/chain": { body: { ...chainGlobal, entries: [entrySonnet] } },
  "GET /api/admin/panel-chain": { body: { ...chainGlobal, entries: [] } },
});

describe("AdminSettings comment footer", () => {
  it("shows the default as a hint, and sends the text trimmed", async () => {
    const fake = fakeApi({
      ...routes(),
      "PUT /api/admin/comment-footer": { body: { ...commentFooter, note: "Run by the team" } },
    });
    renderApp("/admin/settings");
    const form = await screen.findByRole("form", { name: "Comment footer" });
    const input = within(form).getByLabelText("Text");
    expect(input).toHaveValue("");
    expect(input).toHaveAttribute("placeholder", commentFooter.default);
    await userEvent.type(input, "  Run by the team ");
    await userEvent.click(within(form).getByRole("button", { name: "Save footer" }));
    expect(await within(form).findByRole("status")).toHaveTextContent("Comment footer saved.");
    const [put] = fake.find("PUT", "/api/admin/comment-footer");
    expect(put.headers.get("X-CSRF-Token")).toBe("csrf-123");
    expect(put.body).toEqual({ note: "Run by the team" });
    expect(input).toHaveValue("Run by the team");
  });

  it("sends an empty text to go back to the default", async () => {
    const fake = fakeApi({
      ...routes(),
      "GET /api/admin/comment-footer": { body: { ...commentFooter, note: "Run by the team" } },
      "PUT /api/admin/comment-footer": { body: commentFooter },
    });
    renderApp("/admin/settings");
    const form = await screen.findByRole("form", { name: "Comment footer" });
    await userEvent.clear(within(form).getByLabelText("Text"));
    await userEvent.click(within(form).getByRole("button", { name: "Save footer" }));
    await within(form).findByRole("status");
    expect(fake.find("PUT", "/api/admin/comment-footer")[0].body).toEqual({ note: "" });
  });

  it("shows why the server refused the text", async () => {
    fakeApi({
      ...routes(),
      "PUT /api/admin/comment-footer": { status: 400, body: { error: "The footer text can't contain <." } },
    });
    renderApp("/admin/settings");
    const form = await screen.findByRole("form", { name: "Comment footer" });
    await userEvent.type(within(form).getByLabelText("Text"), "a <b>");
    await userEvent.click(within(form).getByRole("button", { name: "Save footer" }));
    expect(await within(form).findByText(/can't contain </)).toBeInTheDocument();
  });
});

describe("AdminSettings defaults", () => {
  it("shows the defaults in a form an admin can change", async () => {
    fakeApi(routes());
    renderApp("/admin/settings");
    expect(await screen.findByRole("heading", { name: "Settings" })).toBeInTheDocument();
    const form = await screen.findByRole("form", { name: "Repository defaults" });
    expect(within(form).getByRole("switch", { name: "Approve clean reviews" })).toBeChecked();
    expect(within(form).getByRole("switch", { name: "Review drafts" })).toBeChecked();
    expect(within(form).getByLabelText("Status when a review fails")).toHaveValue("failed");
    expect(within(form).getByLabelText("Timeout (seconds)")).toHaveValue("600");
    expect(within(form).getByLabelText("Max steps")).toHaveValue("40");
    expect(
      within(form).getByText("Steps each opencode run may take before pergent stops it; each step can call several tools."),
    ).toBeInTheDocument();
    expect(within(form).getByLabelText("Memory limit")).toHaveValue("");
    expect(within(form).getByLabelText("Re-run cap per MR/PR (per day)")).toHaveValue(10);
    expect(within(form).getByLabelText("Re-run cap per repository (per day)")).toHaveValue(50);
  });

  it("sends every default with a PUT, with the CSRF token", async () => {
    const fake = fakeApi({
      ...routes(),
      "PUT /api/admin/defaults": { body: { ...defaults, approve: false, jobTimeoutSeconds: 900, jobMemory: "6Gi" } },
    });
    renderApp("/admin/settings");
    const form = await screen.findByRole("form", { name: "Repository defaults" });
    await userEvent.click(within(form).getByRole("switch", { name: "Approve clean reviews" }));
    await userEvent.clear(within(form).getByLabelText("Timeout (seconds)"));
    await userEvent.type(within(form).getByLabelText("Timeout (seconds)"), "900");
    await userEvent.type(within(form).getByLabelText("Memory limit"), "6Gi");
    await userEvent.click(within(form).getByRole("button", { name: "Save defaults" }));
    const [put] = fake.find("PUT", "/api/admin/defaults");
    expect(put.headers.get("X-CSRF-Token")).toBe("csrf-123");
    expect(put.body).toEqual({
      approve: false,
      // Untouched, so equal to the config: null keeps them following it.
      reviewDrafts: null,
      inlineFindings: null,
      commandsAuthorAndMaintainersOnly: null,
      incrementalReviews: null,
      learnFromDismissals: null,
      quietReviews: null,
      linkedReposEnabled: null,
      commentLayout: null,
      failureStatus: null,
      jobTimeoutSeconds: 900,
      maxTurns: null,
      jobMemory: "6Gi",
      rerunLimitPerMR: null,
      rerunLimitPerRepo: null,
      // Untouched too: the default skills are sent as they are.
      skills: ["code-review"],
    });
    const saved = await screen.findByText("Defaults saved.");
    expect(saved).not.toHaveClass("problem");
  });

  it("shows Inline findings after Review drafts and sends a change with the other defaults", async () => {
    const fake = fakeApi({
      ...routes(),
      "PUT /api/admin/defaults": { body: { ...defaults, inlineFindings: false } },
    });
    renderApp("/admin/settings");
    const form = await screen.findByRole("form", { name: "Repository defaults" });
    expect(
      within(form).getByText(
        "Post each finding as a comment on its lines, with a one-click fix when it is small. Off keeps every finding in the review comment.",
      ),
    ).toBeInTheDocument();
    const names = within(form)
      .getAllByRole("switch")
      .map((el) => el.getAttribute("aria-label") ?? el.textContent);
    expect(names.indexOf("Inline findings")).toBe(names.indexOf("Review drafts") + 1);
    const toggle = within(form).getByRole("switch", { name: "Inline findings" });
    expect(toggle).toBeChecked();
    await userEvent.click(toggle);
    await userEvent.click(within(form).getByRole("button", { name: "Save defaults" }));
    expect(fake.find("PUT", "/api/admin/defaults")[0].body).toMatchObject({ reviewDrafts: null, inlineFindings: false });
    expect(await screen.findByText("Defaults saved.")).toBeInTheDocument();
    expect(within(form).getByRole("switch", { name: "Inline findings" })).not.toBeChecked();
  });

  it("shows the commands switch after Inline findings, sends a change and nulls it when it equals the config", async () => {
    const fake = fakeApi({
      ...routes(),
      "PUT /api/admin/defaults": { body: { ...defaults, commandsAuthorAndMaintainersOnly: false } },
    });
    renderApp("/admin/settings");
    const form = await screen.findByRole("form", { name: "Repository defaults" });
    expect(within(form).getByText(/When on, only the author of a change and the repository's maintainers and owners can use pergent commands on it. When off, anyone with write access can./)).toBeInTheDocument();
    const names = within(form)
      .getAllByRole("switch")
      .map((el) => el.getAttribute("aria-label") ?? el.textContent);
    expect(names.indexOf("Only the author and maintainers use commands")).toBe(names.indexOf("Inline findings") + 1);
    const toggle = within(form).getByRole("switch", { name: "Only the author and maintainers use commands" });
    expect(toggle).toBeChecked();
    await userEvent.click(toggle);
    await userEvent.click(within(form).getByRole("button", { name: "Save defaults" }));
    expect(fake.find("PUT", "/api/admin/defaults")[0].body).toMatchObject({ commandsAuthorAndMaintainersOnly: false });
    expect(await screen.findByText("Defaults saved.")).toBeInTheDocument();
    expect(within(form).getByRole("switch", { name: "Only the author and maintainers use commands" })).not.toBeChecked();
  });

  it("shows the incremental reviews switch after the commands switch, sends a change and nulls it when it equals the config", async () => {
    const fake = fakeApi({
      ...routes(),
      "PUT /api/admin/defaults": { body: { ...defaults, incrementalReviews: false } },
    });
    renderApp("/admin/settings");
    const form = await screen.findByRole("form", { name: "Repository defaults" });
    expect(within(form).getByText(/After a push, review only the files changed since the last review./)).toBeInTheDocument();
    const names = within(form)
      .getAllByRole("switch")
      .map((el) => el.getAttribute("aria-label") ?? el.textContent);
    expect(names.indexOf("Incremental reviews")).toBe(names.indexOf("Only the author and maintainers use commands") + 1);
    const toggle = within(form).getByRole("switch", { name: "Incremental reviews" });
    expect(toggle).toBeChecked();
    await userEvent.click(toggle);
    await userEvent.click(within(form).getByRole("button", { name: "Save defaults" }));
    expect(fake.find("PUT", "/api/admin/defaults")[0].body).toMatchObject({ incrementalReviews: false, commandsAuthorAndMaintainersOnly: null });
    expect(await screen.findByText("Defaults saved.")).toBeInTheDocument();
    expect(within(form).getByRole("switch", { name: "Incremental reviews" })).not.toBeChecked();
  });

  it("shows the learn from dismissals switch after the incremental reviews switch, sends a change and nulls it when it equals the config", async () => {
    const fake = fakeApi({
      ...routes(),
      "PUT /api/admin/defaults": { body: { ...defaults, learnFromDismissals: false } },
    });
    renderApp("/admin/settings");
    const form = await screen.findByRole("form", { name: "Repository defaults" });
    expect(within(form).getByText(/Later reviews are told what people with write access rejected, and why./)).toBeInTheDocument();
    const names = within(form)
      .getAllByRole("switch")
      .map((el) => el.getAttribute("aria-label") ?? el.textContent);
    expect(names.indexOf("Learn from dismissals")).toBe(names.indexOf("Incremental reviews") + 1);
    const toggle = within(form).getByRole("switch", { name: "Learn from dismissals" });
    expect(toggle).toBeChecked();
    await userEvent.click(toggle);
    await userEvent.click(within(form).getByRole("button", { name: "Save defaults" }));
    expect(fake.find("PUT", "/api/admin/defaults")[0].body).toMatchObject({ learnFromDismissals: false, incrementalReviews: null });
    expect(await screen.findByText("Defaults saved.")).toBeInTheDocument();
    expect(within(form).getByRole("switch", { name: "Learn from dismissals" })).not.toBeChecked();
  });

  it("shows the quiet reviews switch after the learn from dismissals switch, sends a change and nulls it when it equals the config", async () => {
    const fake = fakeApi({
      ...routes(),
      "PUT /api/admin/defaults": { body: { ...defaults, quietReviews: true } },
    });
    renderApp("/admin/settings");
    const form = await screen.findByRole("form", { name: "Repository defaults" });
    expect(
      within(form).getByText(/Fewer nitpicks, no suggestion threads and a shorter comment. Blocking findings, failures and approval stay the same./),
    ).toBeInTheDocument();
    const names = within(form)
      .getAllByRole("switch")
      .map((el) => el.getAttribute("aria-label") ?? el.textContent);
    expect(names.indexOf("Quiet reviews")).toBe(names.indexOf("Learn from dismissals") + 1);
    const toggle = within(form).getByRole("switch", { name: "Quiet reviews" });
    expect(toggle).not.toBeChecked();
    await userEvent.click(toggle);
    await userEvent.click(within(form).getByRole("button", { name: "Save defaults" }));
    expect(fake.find("PUT", "/api/admin/defaults")[0].body).toMatchObject({ quietReviews: true, learnFromDismissals: null });
    expect(await screen.findByText("Defaults saved.")).toBeInTheDocument();
    expect(within(form).getByRole("switch", { name: "Quiet reviews" })).toBeChecked();
  });

  it("shows the linked repositories switch, off by default, and sends only the on/off default", async () => {
    const fake = fakeApi({
      ...routes(),
      "PUT /api/admin/defaults": { body: { ...defaults, linkedReposEnabled: true, pinned: { linkedReposEnabled: true } } },
    });
    renderApp("/admin/settings");
    const form = await screen.findByRole("form", { name: "Repository defaults" });
    const row = within(form).getByText("Linked repositories", { selector: ".name" }).closest(".set") as HTMLElement;
    expect(row).toHaveTextContent("Off by default");
    // The links are not a default.
    expect(row).toHaveTextContent("set on each repository's page");
    expect(within(row).queryByRole("textbox")).not.toBeInTheDocument();
    const toggle = within(row).getByRole("switch", { name: "Linked repositories" });
    expect(toggle).not.toBeChecked();
    await userEvent.click(toggle);
    await userEvent.click(within(form).getByRole("button", { name: "Save defaults" }));
    const body = fake.find("PUT", "/api/admin/defaults")[0].body as Record<string, unknown>;
    expect(body).toMatchObject({ linkedReposEnabled: true });
    expect(body).not.toHaveProperty("linkedRepos");
    expect(await screen.findByText("Defaults saved.")).toBeInTheDocument();
    expect(within(form).getByRole("switch", { name: "Linked repositories" })).toBeChecked();
    expect(within(form).getByRole("switch", { name: "Linked repositories" }).closest(".set")).toHaveTextContent("Saved here");
  });

  it("shows the comment layout after the quiet reviews switch and sends a changed list with the other defaults", async () => {
    const fake = fakeApi({
      ...routes(),
      "PUT /api/admin/defaults": { body: defaults },
    });
    renderApp("/admin/settings");
    const form = await screen.findByRole("form", { name: "Repository defaults" });
    const row = within(form).getByText("Comment layout", { selector: ".name" }).closest(".set") as HTMLElement;
    expect(within(form).getByText("Quiet reviews", { selector: ".name" }).closest(".set")?.nextElementSibling).toBe(row);
    const names = within(row).getAllByRole("listitem").map((r) => within(r).getByRole("combobox").getAttribute("aria-label"));
    expect(names).toEqual(["Summary state", "Findings state", "Dismissed state", "Checks state", "Files state"]);
    await userEvent.click(within(row).getByRole("button", { name: "Move Files up" }));
    await userEvent.click(within(row).getByRole("checkbox", { name: "Show Dismissed" }));
    await userEvent.click(within(form).getByRole("button", { name: "Save defaults" }));
    expect(fake.find("PUT", "/api/admin/defaults")[0].body).toMatchObject({
      commentLayout: ["summary:open", "findings:collapsed", "files:collapsed", "checks:open"],
    });
  });

  it("sends null for a layout moved back to the built-in one", async () => {
    const fake = fakeApi({
      ...routes(),
      "PUT /api/admin/defaults": { body: defaults },
    });
    renderApp("/admin/settings");
    const form = await screen.findByRole("form", { name: "Repository defaults" });
    const row = within(form).getByText("Comment layout", { selector: ".name" }).closest(".set") as HTMLElement;
    await userEvent.click(within(row).getByRole("button", { name: "Move Files up" }));
    await userEvent.click(within(row).getByRole("button", { name: "Move Files down" }));
    await userEvent.click(within(form).getByRole("button", { name: "Save defaults" }));
    expect(fake.find("PUT", "/api/admin/defaults")[0].body).toMatchObject({ commentLayout: null });
  });

  it("shows a saved layout as saved here, and sends null for Reset to default", async () => {
    const fake = fakeApi({
      ...routes(),
      "GET /api/admin/defaults": {
        body: { ...defaults, commentLayout: ["findings:open"], pinned: { commentLayout: true } },
      },
      "PUT /api/admin/defaults": { body: defaults },
    });
    renderApp("/admin/settings");
    const form = await screen.findByRole("form", { name: "Repository defaults" });
    const row = within(form).getByText("Comment layout", { selector: ".name" }).closest(".set") as HTMLElement;
    expect(within(row).getByText(/Saved here: repositories switched on in the dashboard start with this layout./)).toBeInTheDocument();
    expect(within(row).getAllByRole("listitem").map((r) => within(r).getByRole("combobox").getAttribute("aria-label"))).toEqual([
      "Findings state",
      "Summary state",
      "Dismissed state",
      "Checks state",
      "Files state",
    ]);
    await userEvent.click(within(row).getByRole("button", { name: "Reset to default" }));
    await userEvent.click(within(form).getByRole("button", { name: "Save defaults" }));
    expect(fake.find("PUT", "/api/admin/defaults")[0].body).toMatchObject({ commentLayout: null });
  });

  it("saves changed re-run caps", async () => {
    const fake = fakeApi({
      ...routes(),
      "PUT /api/admin/defaults": { body: { ...defaults, rerunLimitPerMR: 3, rerunLimitPerRepo: 120 } },
    });
    renderApp("/admin/settings");
    const form = await screen.findByRole("form", { name: "Repository defaults" });
    const perMR = within(form).getByLabelText("Re-run cap per MR/PR (per day)");
    const perRepo = within(form).getByLabelText("Re-run cap per repository (per day)");
    await userEvent.clear(perMR);
    await userEvent.type(perMR, "3");
    await userEvent.clear(perRepo);
    await userEvent.type(perRepo, "120");
    await userEvent.click(within(form).getByRole("button", { name: "Save defaults" }));
    expect(fake.find("PUT", "/api/admin/defaults")[0].body).toMatchObject({ rerunLimitPerMR: 3, rerunLimitPerRepo: 120 });
    expect(await screen.findByText("Defaults saved.")).toBeInTheDocument();
    // The saved answer is what the form shows.
    expect(within(form).getByLabelText("Re-run cap per MR/PR (per day)")).toHaveValue(3);
    expect(within(form).getByLabelText("Re-run cap per repository (per day)")).toHaveValue(120);
  });

  it("saving untouched defaults sends null for values equal to the config", async () => {
    const fake = fakeApi({ ...routes(), "PUT /api/admin/defaults": { body: defaults } });
    renderApp("/admin/settings");
    const form = await screen.findByRole("form", { name: "Repository defaults" });
    await userEvent.click(within(form).getByRole("button", { name: "Save defaults" }));
    expect(fake.find("PUT", "/api/admin/defaults")[0].body).toEqual({
      approve: null,
      reviewDrafts: null,
      inlineFindings: null,
      commandsAuthorAndMaintainersOnly: null,
      incrementalReviews: null,
      learnFromDismissals: null,
      quietReviews: null,
      linkedReposEnabled: null,
      commentLayout: null,
      failureStatus: null,
      jobTimeoutSeconds: null,
      maxTurns: null,
      jobMemory: null,
      rerunLimitPerMR: null,
      rerunLimitPerRepo: null,
      skills: ["code-review"],
    });
  });

  it("says under each default what the config gives", async () => {
    fakeApi(routes());
    renderApp("/admin/settings");
    const form = await screen.findByRole("form", { name: "Repository defaults" });
    const row = (name: string) => within(form).getByText(name, { selector: ".name" }).closest(".set") as HTMLElement;
    expect(row("Approve clean reviews")).toHaveTextContent("From config: on");
    expect(row("Review drafts")).toHaveTextContent("From config: on");
    expect(row("Inline findings")).toHaveTextContent("From config: on");
    expect(row("Only the author and maintainers use commands")).toHaveTextContent("From config: on");
    expect(row("Incremental reviews")).toHaveTextContent("From config: on");
    expect(row("Learn from dismissals")).toHaveTextContent("From config: on");
    expect(row("Quiet reviews")).toHaveTextContent("From config: off");
    expect(row("Linked repositories")).toHaveTextContent("From config: off");
    expect(row("Comment layout")).toHaveTextContent("Not saved here: repositories switched on in the dashboard start with the built-in layout.");
    expect(row("Status when a review fails")).toHaveTextContent("From config: failed (blocks merge)");
    expect(row("Timeout")).toHaveTextContent("From config: 600");
    expect(row("Max steps")).toHaveTextContent("From config: 40");
    expect(row("Memory limit")).toHaveTextContent("From config: none");
    expect(row("Re-run cap per MR/PR")).toHaveTextContent("From config: 10");
    expect(row("Re-run cap per repository")).toHaveTextContent("From config: 50");
    expect(within(form).queryByRole("button", { name: /^Use config value/ })).not.toBeInTheDocument();
  });

  const pinnedTimeout = {
    ...defaults,
    jobTimeoutSeconds: 600,
    config: { ...defaults.config, jobTimeoutSeconds: 1800 },
    pinned: { jobTimeoutSeconds: true },
  };

  it("shows a pinned default against the config's value, and Use config value sends null for it", async () => {
    const fake = fakeApi({
      ...routes(),
      "GET /api/admin/defaults": { body: pinnedTimeout },
      "PUT /api/admin/defaults": { body: defaults },
    });
    renderApp("/admin/settings");
    const form = await screen.findByRole("form", { name: "Repository defaults" });
    const row = within(form).getByText("Timeout", { selector: ".name" }).closest(".set") as HTMLElement;
    expect(within(row).getByText("Saved here; the config says 1800.")).toBeInTheDocument();
    expect(row).not.toHaveTextContent("From config");
    expect(within(form).getAllByRole("button", { name: /^Use config value/ })).toHaveLength(1);
    expect(within(form).getByLabelText("Timeout (seconds)")).toHaveValue("600");
    await userEvent.click(within(row).getByRole("button", { name: /^Use config value/ }));
    expect(within(form).getByLabelText("Timeout (seconds)")).toHaveValue("1800");
    await userEvent.click(within(form).getByRole("button", { name: "Save defaults" }));
    expect(fake.find("PUT", "/api/admin/defaults")[0].body).toMatchObject({ jobTimeoutSeconds: null });
  });

  it("shows a pinned commands switch against the config, and Use config value sends null for it", async () => {
    const fake = fakeApi({
      ...routes(),
      "GET /api/admin/defaults": {
        body: { ...defaults, commandsAuthorAndMaintainersOnly: false, pinned: { commandsAuthorAndMaintainersOnly: true } },
      },
      "PUT /api/admin/defaults": { body: defaults },
    });
    renderApp("/admin/settings");
    const form = await screen.findByRole("form", { name: "Repository defaults" });
    const row = within(form).getByText("Only the author and maintainers use commands", { selector: ".name" }).closest(".set") as HTMLElement;
    expect(within(row).getByText("Saved here; the config says on.")).toBeInTheDocument();
    expect(within(row).getByRole("switch", { name: "Only the author and maintainers use commands" })).not.toBeChecked();
    await userEvent.click(within(row).getByRole("button", { name: /^Use config value/ }));
    expect(within(row).getByRole("switch", { name: "Only the author and maintainers use commands" })).toBeChecked();
    await userEvent.click(within(form).getByRole("button", { name: "Save defaults" }));
    expect(fake.find("PUT", "/api/admin/defaults")[0].body).toMatchObject({ commandsAuthorAndMaintainersOnly: null });
  });

  it("shows a pinned incremental reviews switch against the config, and Use config value sends null for it", async () => {
    const fake = fakeApi({
      ...routes(),
      "GET /api/admin/defaults": {
        body: { ...defaults, incrementalReviews: false, pinned: { incrementalReviews: true } },
      },
      "PUT /api/admin/defaults": { body: defaults },
    });
    renderApp("/admin/settings");
    const form = await screen.findByRole("form", { name: "Repository defaults" });
    const row = within(form).getByText("Incremental reviews", { selector: ".name" }).closest(".set") as HTMLElement;
    expect(within(row).getByText("Saved here; the config says on.")).toBeInTheDocument();
    expect(within(row).getByRole("switch", { name: "Incremental reviews" })).not.toBeChecked();
    await userEvent.click(within(row).getByRole("button", { name: /^Use config value/ }));
    expect(within(row).getByRole("switch", { name: "Incremental reviews" })).toBeChecked();
    await userEvent.click(within(form).getByRole("button", { name: "Save defaults" }));
    expect(fake.find("PUT", "/api/admin/defaults")[0].body).toMatchObject({ incrementalReviews: null });
  });

  it("shows a pinned learn from dismissals switch against the config, and Use config value sends null for it", async () => {
    const fake = fakeApi({
      ...routes(),
      "GET /api/admin/defaults": {
        body: { ...defaults, learnFromDismissals: false, pinned: { learnFromDismissals: true } },
      },
      "PUT /api/admin/defaults": { body: defaults },
    });
    renderApp("/admin/settings");
    const form = await screen.findByRole("form", { name: "Repository defaults" });
    const row = within(form).getByText("Learn from dismissals", { selector: ".name" }).closest(".set") as HTMLElement;
    expect(within(row).getByText("Saved here; the config says on.")).toBeInTheDocument();
    expect(within(row).getByRole("switch", { name: "Learn from dismissals" })).not.toBeChecked();
    await userEvent.click(within(row).getByRole("button", { name: /^Use config value/ }));
    expect(within(row).getByRole("switch", { name: "Learn from dismissals" })).toBeChecked();
    await userEvent.click(within(form).getByRole("button", { name: "Save defaults" }));
    expect(fake.find("PUT", "/api/admin/defaults")[0].body).toMatchObject({ learnFromDismissals: null });
  });

  it("shows a pinned quiet reviews switch against the config, and Use config value sends null for it", async () => {
    const fake = fakeApi({
      ...routes(),
      "GET /api/admin/defaults": {
        body: { ...defaults, quietReviews: true, pinned: { quietReviews: true } },
      },
      "PUT /api/admin/defaults": { body: defaults },
    });
    renderApp("/admin/settings");
    const form = await screen.findByRole("form", { name: "Repository defaults" });
    const row = within(form).getByText("Quiet reviews", { selector: ".name" }).closest(".set") as HTMLElement;
    expect(within(row).getByText("Saved here; the config says off.")).toBeInTheDocument();
    expect(within(row).getByRole("switch", { name: "Quiet reviews" })).toBeChecked();
    await userEvent.click(within(row).getByRole("button", { name: /^Use config value/ }));
    expect(within(row).getByRole("switch", { name: "Quiet reviews" })).not.toBeChecked();
    await userEvent.click(within(form).getByRole("button", { name: "Save defaults" }));
    expect(fake.find("PUT", "/api/admin/defaults")[0].body).toMatchObject({ quietReviews: null });
  });

  it("keeps a pinned value that differs from the config when saving", async () => {
    const fake = fakeApi({
      ...routes(),
      "GET /api/admin/defaults": { body: pinnedTimeout },
      "PUT /api/admin/defaults": { body: pinnedTimeout },
    });
    renderApp("/admin/settings");
    const form = await screen.findByRole("form", { name: "Repository defaults" });
    await userEvent.click(within(form).getByRole("button", { name: "Save defaults" }));
    expect(fake.find("PUT", "/api/admin/defaults")[0].body).toMatchObject({ jobTimeoutSeconds: 600, maxTurns: null });
  });

  it("lets a config timeout outside 60-7200 be saved untouched or typed, as null", async () => {
    const long = { ...defaults, jobTimeoutSeconds: 10800, config: { ...defaults.config, jobTimeoutSeconds: 10800 } };
    const fake = fakeApi({ ...routes(), "GET /api/admin/defaults": { body: long }, "PUT /api/admin/defaults": { body: long } });
    renderApp("/admin/settings");
    const form = await screen.findByRole("form", { name: "Repository defaults" });
    await userEvent.click(within(form).getByRole("button", { name: "Save defaults" }));
    await waitFor(() => expect(fake.find("PUT", "/api/admin/defaults")).toHaveLength(1));
    expect(fake.find("PUT", "/api/admin/defaults")[0].body).toMatchObject({ jobTimeoutSeconds: null });
    await userEvent.clear(within(form).getByLabelText("Timeout (seconds)"));
    await userEvent.type(within(form).getByLabelText("Timeout (seconds)"), "10800");
    await userEvent.click(within(form).getByRole("button", { name: "Save defaults" }));
    await waitFor(() => expect(fake.find("PUT", "/api/admin/defaults")).toHaveLength(2));
    expect(fake.find("PUT", "/api/admin/defaults")[1].body).toMatchObject({ jobTimeoutSeconds: null });
  });

  it("still refuses a timeout over 7200 that differs from the config", async () => {
    const fake = fakeApi(routes());
    renderApp("/admin/settings");
    const form = await screen.findByRole("form", { name: "Repository defaults" });
    await userEvent.clear(within(form).getByLabelText("Timeout (seconds)"));
    await userEvent.type(within(form).getByLabelText("Timeout (seconds)"), "7201");
    await userEvent.click(within(form).getByRole("button", { name: "Save defaults" }));
    expect(within(form).getByText(/from 60 to 7200/)).toBeInTheDocument();
    expect(fake.calls.filter((c) => c.method === "PUT")).toHaveLength(0);
  });

  it("sends a changed value as it is", async () => {
    const fake = fakeApi({ ...routes(), "PUT /api/admin/defaults": { body: { ...defaults, jobTimeoutSeconds: 900 } } });
    renderApp("/admin/settings");
    const form = await screen.findByRole("form", { name: "Repository defaults" });
    await userEvent.clear(within(form).getByLabelText("Timeout (seconds)"));
    await userEvent.type(within(form).getByLabelText("Timeout (seconds)"), "900");
    await userEvent.click(within(form).getByRole("button", { name: "Save defaults" }));
    expect(fake.find("PUT", "/api/admin/defaults")[0].body).toMatchObject({ jobTimeoutSeconds: 900, maxTurns: null });
  });

  it("sends an emptied re-run cap as null, the built-in default", async () => {
    const fake = fakeApi({ ...routes(), "PUT /api/admin/defaults": { body: defaults } });
    renderApp("/admin/settings");
    const form = await screen.findByRole("form", { name: "Repository defaults" });
    await userEvent.clear(within(form).getByLabelText("Re-run cap per repository (per day)"));
    await userEvent.click(within(form).getByRole("button", { name: "Save defaults" }));
    expect(fake.find("PUT", "/api/admin/defaults")[0].body).toMatchObject({ rerunLimitPerMR: null, rerunLimitPerRepo: null });
  });

  it.each(["0", "1001", "1.5"])("refuses the re-run cap %s without asking the server", async (value) => {
    const fake = fakeApi(routes());
    renderApp("/admin/settings");
    const form = await screen.findByRole("form", { name: "Repository defaults" });
    const perMR = within(form).getByLabelText("Re-run cap per MR/PR (per day)");
    await userEvent.clear(perMR);
    await userEvent.type(perMR, value);
    await userEvent.click(within(form).getByRole("button", { name: "Save defaults" }));
    expect(within(form).getByText("The re-run caps are whole numbers from 1 to 1000.")).toBeInTheDocument();
    expect(fake.calls.filter((c) => c.method === "PUT")).toHaveLength(0);
  });

  it("sends an empty memory limit as null, the built-in default", async () => {
    const fake = fakeApi({ ...routes(), "PUT /api/admin/defaults": { body: defaults } });
    renderApp("/admin/settings");
    const form = await screen.findByRole("form", { name: "Repository defaults" });
    await userEvent.click(within(form).getByRole("button", { name: "Save defaults" }));
    expect(fake.find("PUT", "/api/admin/defaults")[0].body).toMatchObject({ jobMemory: null });
  });

  it.each(["abc", "59", "7201"])("refuses a timeout of %s without asking the server", async (text) => {
    const fake = fakeApi(routes());
    renderApp("/admin/settings");
    const form = await screen.findByRole("form", { name: "Repository defaults" });
    await userEvent.clear(within(form).getByLabelText("Timeout (seconds)"));
    await userEvent.type(within(form).getByLabelText("Timeout (seconds)"), text);
    await userEvent.click(within(form).getByRole("button", { name: "Save defaults" }));
    expect(within(form).getByText("The timeout is a whole number of seconds, from 60 to 7200.")).toBeInTheDocument();
    expect(fake.calls.filter((c) => c.method === "PUT")).toHaveLength(0);
  });

  it("refuses a timeout that isn't a whole number without asking the server", async () => {
    const fake = fakeApi(routes());
    renderApp("/admin/settings");
    const form = await screen.findByRole("form", { name: "Repository defaults" });
    await userEvent.clear(within(form).getByLabelText("Timeout (seconds)"));
    await userEvent.click(within(form).getByRole("button", { name: "Save defaults" }));
    expect(within(form).getByText(/whole number of seconds/)).toBeInTheDocument();
    expect(fake.calls.filter((c) => c.method === "PUT")).toHaveLength(0);
  });

  it("sends a changed Max steps with the other defaults", async () => {
    const fake = fakeApi({ ...routes(), "PUT /api/admin/defaults": { body: { ...defaults, maxTurns: 90 } } });
    renderApp("/admin/settings");
    const form = await screen.findByRole("form", { name: "Repository defaults" });
    await userEvent.clear(within(form).getByLabelText("Max steps"));
    await userEvent.type(within(form).getByLabelText("Max steps"), "90");
    await userEvent.click(within(form).getByRole("button", { name: "Save defaults" }));
    expect(fake.find("PUT", "/api/admin/defaults")[0].body).toMatchObject({ maxTurns: 90, jobTimeoutSeconds: null });
  });

  it.each(["4", "201", ""])("refuses Max steps %j without asking the server", async (text) => {
    const fake = fakeApi(routes());
    renderApp("/admin/settings");
    const form = await screen.findByRole("form", { name: "Repository defaults" });
    await userEvent.clear(within(form).getByLabelText("Max steps"));
    if (text !== "") await userEvent.type(within(form).getByLabelText("Max steps"), text);
    await userEvent.click(within(form).getByRole("button", { name: "Save defaults" }));
    expect(within(form).getByText("Max steps is a whole number from 5 to 200.")).toBeInTheDocument();
    expect(fake.calls.filter((c) => c.method === "PUT")).toHaveLength(0);
  });

  it("shows the server's text when the defaults are refused", async () => {
    fakeApi({
      ...routes(),
      "PUT /api/admin/defaults": { status: 400, body: { error: "jobMemory must be between 256Mi and 32Gi" } },
    });
    renderApp("/admin/settings");
    const form = await screen.findByRole("form", { name: "Repository defaults" });
    await userEvent.type(within(form).getByLabelText("Memory limit"), "1Mi");
    await userEvent.click(within(form).getByRole("button", { name: "Save defaults" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("jobMemory must be between 256Mi and 32Gi");
  });
});

describe("AdminSettings default skills", () => {
  it("shows a running total against 900 KiB, and sends the ticked skills", async () => {
    const fake = fakeApi({
      ...routes(),
      "PUT /api/admin/defaults": { body: { ...defaults, skills: ["code-review", "security-review"] } },
    });
    renderApp("/admin/settings");
    const picker = await screen.findByRole("group", { name: "Default skills" });
    expect(within(picker).getByRole("switch", { name: "Default skill code-review" })).toBeChecked();
    expect(within(picker).getByRole("switch", { name: "Default skill security-review" })).not.toBeChecked();
    expect(within(picker).getByText("Total 52 B of 900 KiB")).toBeInTheDocument();

    await userEvent.click(within(picker).getByRole("switch", { name: "Default skill security-review" }));
    expect(within(picker).getByText("Total 113 B of 900 KiB")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Save defaults" }));
    expect(fake.find("PUT", "/api/admin/defaults")[0].body).toMatchObject({ skills: ["code-review", "security-review"] });
    expect(await screen.findByText("Defaults saved.")).toBeInTheDocument();
  });

  it("refetches the library after a save, so default marks and warnings aren't stale", async () => {
    let skillReads = 0;
    const fake = fakeApi({
      ...routes(),
      "GET /api/skills": () => ({ body: skillReads++ === 0 ? librarySkills : librarySkills.map((s) => ({ ...s, inDefaults: true })) }),
      "PUT /api/admin/defaults": { body: { ...defaults, skills: ["code-review", "security-review"] } },
    });
    renderApp("/admin/settings");
    const picker = await screen.findByRole("group", { name: "Default skills" });
    await userEvent.click(within(picker).getByRole("switch", { name: "Default skill security-review" }));
    await userEvent.click(screen.getByRole("button", { name: "Save defaults" }));
    expect(await screen.findByText("Defaults saved.")).toBeInTheDocument();
    await waitFor(() => expect(fake.find("GET", "/api/skills")).toHaveLength(2));
  });

  it("shows a stored default the library no longer has, and drops it on save", async () => {
    const fake = fakeApi({
      ...routes(),
      "GET /api/admin/defaults": { body: { ...defaults, skills: ["code-review", "old-skill"] } },
      "PUT /api/admin/defaults": { body: { ...defaults, skills: ["code-review"] } },
    });
    renderApp("/admin/settings");
    const picker = await screen.findByRole("group", { name: "Default skills" });
    const old = within(picker).getByRole("switch", { name: "Default skill old-skill" });
    expect(old).toBeChecked();
    expect(old.closest(".skill")).toHaveTextContent("not in the library");
    expect(picker).toHaveTextContent("old-skill is no longer in the library");
    // It counts for nothing in the total.
    expect(within(picker).getByText("Total 52 B of 900 KiB")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Save defaults" }));
    expect(fake.find("PUT", "/api/admin/defaults")[0].body).toMatchObject({ skills: ["code-review"] });
  });

  it("flags a total over 900 KiB and shows the server's 400 message", async () => {
    const big = [
      { ...librarySkills[0], size: 600 * 1024 },
      { ...librarySkills[1], size: 400 * 1024, inDefaults: true },
    ];
    fakeApi({
      ...routes(),
      "GET /api/skills": { body: big },
      "GET /api/admin/defaults": { body: { ...defaults, skills: ["code-review", "security-review"] } },
      "PUT /api/admin/defaults": {
        status: 400,
        body: { error: "the library skills come to 1000 KiB; a review takes at most 900 KiB" },
      },
    });
    renderApp("/admin/settings");
    const picker = await screen.findByRole("group", { name: "Default skills" });
    const total = within(picker).getByText("Total 1000 KiB of 900 KiB");
    expect(total).toHaveClass("problem");
    await userEvent.click(screen.getByRole("button", { name: "Save defaults" }));
    expect(await screen.findByRole("alert")).toHaveTextContent(
      "the library skills come to 1000 KiB; a review takes at most 900 KiB",
    );
  });

  it("can save an empty default list, and says what that means", async () => {
    const fake = fakeApi({ ...routes(), "PUT /api/admin/defaults": { body: { ...defaults, skills: [] } } });
    renderApp("/admin/settings");
    const picker = await screen.findByRole("group", { name: "Default skills" });
    await userEvent.click(within(picker).getByRole("switch", { name: "Default skill code-review" }));
    expect(within(picker).getByText(/gets no skills/)).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Save defaults" }));
    expect(fake.find("PUT", "/api/admin/defaults")[0].body).toMatchObject({ skills: [] });
  });

  it("leaves the default list out of the save when the library can't be read", async () => {
    const fake = fakeApi({
      ...routes(),
      "GET /api/skills": { status: 500, body: { error: "boom" } },
      "PUT /api/admin/defaults": { body: defaults },
    });
    renderApp("/admin/settings");
    const form = await screen.findByRole("form", { name: "Repository defaults" });
    expect(await within(form).findByText("boom")).toBeInTheDocument();
    await userEvent.click(within(form).getByRole("button", { name: "Save defaults" }));
    // Left out, the API keeps the stored list.
    expect(fake.find("PUT", "/api/admin/defaults")[0].body).not.toHaveProperty("skills");
  });
});

describe("AdminSettings models", () => {
  const listed = (over: Partial<ModelsAdminView> = {}) => ({ ...routes(operator), "GET /api/admin/models": { body: { ...models, ...over } } });
  const entry = (name: string) => screen.findByRole("group", { name });

  it("lists each model by its name, with its provider and model, default mark, base URL, key and chains", async () => {
    fakeApi(listed());
    renderApp("/admin/settings");
    const sonnet = await entry("Claude Sonnet");
    expect(sonnet).toHaveTextContent("anthropic / claude-sonnet");
    expect(within(sonnet).getByText("default")).toBeInTheDocument();
    expect(sonnet).toHaveTextContent("set · changed 3 days ago");
    // The repositories that name no model follow the default.
    expect(sonnet).toHaveTextContent("3 repositories name no model and follow it");
    expect(sonnet).toHaveTextContent("In chains: global chain");
    const opus = await entry("Claude Opus");
    expect(within(opus).queryByText("default")).not.toBeInTheDocument();
    expect(opus).toHaveTextContent("https://llm.example.com/v1");
    expect(opus).toHaveTextContent("In chains: main/acme/payments");
    // No key is ever in the page, nor an input for one until Replace.
    expect(within(opus).queryByLabelText("New API key")).not.toBeInTheDocument();
    expect(within(opus).queryByRole("textbox")).not.toBeInTheDocument();
  });

  it("shows a name, provider or model as text, never as HTML", async () => {
    const evil = { ...modelOpus, id: "m-evil", name: "<b>bold</b>", provider: "<img src=x onerror=alert(1)>", model: "<i>x</i>", usedIn: [] };
    fakeApi(listed({ models: [modelSonnet, evil] }));
    renderApp("/admin/settings");
    const row = await entry("<b>bold</b>");
    expect(row).toHaveTextContent("<img src=x onerror=alert(1)>");
    expect(document.querySelector("img")).toBeNull();
    expect(row.querySelector("i")).toBeNull();
    expect(row.querySelector("b b")).toBeNull();
  });

  it("adds a model with a POST of name, provider, model, base URL, key and teams may use, and lists the new list", async () => {
    let added = false;
    const ds = { ...modelOpus, id: "m-ds", name: "DeepSeek", provider: "deepseek", model: "deepseek-chat", usedIn: [] };
    const fake = fakeApi({
      ...routes(operator),
      "GET /api/admin/models": () => ({ body: added ? { ...models, models: [...models.models, ds] } : models }),
      "POST /api/admin/models": () => {
        added = true;
        return { status: 201, body: ds };
      },
    });
    renderApp("/admin/settings");
    const form = await screen.findByRole("form", { name: "Add a model" });
    const provider = within(form).getByLabelText("Provider");
    const options = within(provider)
      .getAllByRole("option")
      .map((o) => (o as HTMLOptionElement).value);
    expect(options).toEqual(expect.arrayContaining(["anthropic", "openai", "deepseek", "google", "openrouter", "groq", "mistral", "xai", "other"]));
    // Nothing is picked until the admin picks.
    expect(provider).toHaveValue("");
    await userEvent.type(within(form).getByLabelText("Name"), "  DeepSeek ");
    await userEvent.selectOptions(provider, "deepseek");
    await userEvent.type(within(form).getByLabelText("Model"), "deepseek-chat");
    await userEvent.type(within(form).getByLabelText("Base URL"), "https://api.deepseek.example.com");
    const key = within(form).getByLabelText("API key");
    // "off" doesn't stop a password manager filling the admin's own
    // sign-in here; "new-password" and the managers' opt-outs do.
    expect(key).toHaveAttribute("type", "password");
    expect(key).toHaveAttribute("autocomplete", "new-password");
    expect(key).toHaveAttribute("data-1p-ignore");
    expect(key).toHaveAttribute("data-lpignore", "true");
    await userEvent.type(key, "sk-first");
    await userEvent.click(within(form).getByRole("switch", { name: "Teams may use it" }));
    await userEvent.click(within(form).getByRole("button", { name: "Add model" }));
    const [post] = fake.find("POST", "/api/admin/models");
    expect(post.headers.get("X-CSRF-Token")).toBe("csrf-123");
    expect(post.body).toEqual({
      name: "DeepSeek",
      provider: "deepseek",
      model: "deepseek-chat",
      baseURL: "https://api.deepseek.example.com",
      apiKey: "sk-first",
      teamsMayUse: true,
    });
    expect(await entry("DeepSeek")).toBeInTheDocument();
    expect(await within(form).findByRole("status")).toHaveTextContent("Added DeepSeek.");
    // The form is empty again and the key is gone from the page.
    await vi.waitFor(() => expect(within(form).getByLabelText("API key")).toHaveValue(""));
    expect(within(form).getByLabelText("Model")).toHaveValue("");
    expect(within(form).getByLabelText("Name")).toHaveValue("");
    expect(within(form).getByRole("switch", { name: "Teams may use it" })).not.toBeChecked();
    expect(document.body.textContent).not.toContain("sk-first");
  });

  it("takes any other provider ID, and teams may not use the entry unless switched on", async () => {
    const fake = fakeApi({ ...listed(), "POST /api/admin/models": { status: 201, body: modelOpus } });
    renderApp("/admin/settings");
    const form = await screen.findByRole("form", { name: "Add a model" });
    expect(within(form).queryByLabelText("Provider ID")).not.toBeInTheDocument();
    await userEvent.type(within(form).getByLabelText("Name"), "Llama");
    await userEvent.selectOptions(within(form).getByLabelText("Provider"), "other");
    await userEvent.type(within(form).getByLabelText("Provider ID"), "together");
    await userEvent.type(within(form).getByLabelText("Model"), "llama");
    await userEvent.type(within(form).getByLabelText("API key"), "sk-t");
    await userEvent.click(within(form).getByRole("button", { name: "Add model" }));
    expect(fake.find("POST", "/api/admin/models")[0].body).toEqual({
      name: "Llama",
      provider: "together",
      model: "llama",
      baseURL: "",
      apiKey: "sk-t",
      teamsMayUse: false,
    });
  });

  it("leaves the non-secret fields to no password manager either", async () => {
    fakeApi(listed());
    renderApp("/admin/settings");
    const form = await screen.findByRole("form", { name: "Add a model" });
    // A password manager fills a saved username into the text field before
    // a password field: the Base URL once showed "admin".
    for (const label of ["Name", "Model", "Base URL"]) {
      const input = within(form).getByLabelText(label);
      expect(input).toHaveAttribute("autocomplete", "off");
      expect(input).toHaveAttribute("data-1p-ignore");
      expect(input).toHaveAttribute("data-lpignore", "true");
    }
  });

  it("shows the server's text when an add is refused", async () => {
    fakeApi({ ...listed(), "POST /api/admin/models": { status: 409, body: { error: "an entry with this name exists already" } } });
    renderApp("/admin/settings");
    const form = await screen.findByRole("form", { name: "Add a model" });
    await userEvent.type(within(form).getByLabelText("Name"), "Claude Sonnet");
    await userEvent.selectOptions(within(form).getByLabelText("Provider"), "anthropic");
    await userEvent.type(within(form).getByLabelText("Model"), "claude-sonnet");
    await userEvent.type(within(form).getByLabelText("API key"), "sk-x");
    await userEvent.click(within(form).getByRole("button", { name: "Add model" }));
    expect(await within(form).findByRole("alert")).toHaveTextContent("an entry with this name exists already");
  });

  it("changes the name, base URL and key of an entry: provider and model can't be edited", async () => {
    const fake = fakeApi({ ...listed(), "PUT /api/admin/models/m-opus": { body: modelOpus } });
    renderApp("/admin/settings");
    const row = await entry("Claude Opus");
    await userEvent.click(within(row).getByRole("button", { name: "Edit Claude Opus" }));
    const form = screen.getByRole("form", { name: "Edit Claude Opus" });
    expect(within(form).queryByLabelText("Provider")).not.toBeInTheDocument();
    expect(within(form).queryByLabelText("Model")).not.toBeInTheDocument();
    expect(within(form).getByLabelText("Base URL")).toHaveValue("https://llm.example.com/v1");
    // A key that is set is not shown, nor its input, until Replace.
    expect(within(form).queryByLabelText("New API key")).not.toBeInTheDocument();
    await userEvent.clear(within(form).getByLabelText("Name"));
    await userEvent.type(within(form).getByLabelText("Name"), "Opus backup ");
    await userEvent.clear(within(form).getByLabelText("Base URL"));
    await userEvent.click(within(form).getByRole("button", { name: "Save" }));
    // An empty base URL clears it; no key was typed, so none is sent.
    const [put] = fake.find("PUT", "/api/admin/models/m-opus");
    expect(put.headers.get("X-CSRF-Token")).toBe("csrf-123");
    expect(put.body).toEqual({ name: "Opus backup", baseURL: "" });
    await vi.waitFor(() => expect(screen.queryByRole("form", { name: "Edit Claude Opus" })).not.toBeInTheDocument());
  });

  it("sends a replacement key from an empty password input, and forgets it", async () => {
    const fake = fakeApi({ ...listed(), "PUT /api/admin/models/m-opus": { body: modelOpus } });
    renderApp("/admin/settings");
    const row = await entry("Claude Opus");
    await userEvent.click(within(row).getByRole("button", { name: "Edit Claude Opus" }));
    const form = screen.getByRole("form", { name: "Edit Claude Opus" });
    await userEvent.click(within(form).getByRole("button", { name: "Replace" }));
    const key = within(form).getByLabelText("New API key");
    expect(key).toHaveAttribute("type", "password");
    expect(key).toHaveAttribute("autocomplete", "new-password");
    expect(key).toHaveValue("");
    await userEvent.type(key, "sk-new-key");
    await userEvent.click(within(form).getByRole("button", { name: "Save" }));
    expect(fake.find("PUT", "/api/admin/models/m-opus")[0].body).toEqual({
      name: "Claude Opus",
      baseURL: "https://llm.example.com/v1",
      apiKey: "sk-new-key",
    });
    await vi.waitFor(() => expect(screen.queryByLabelText("New API key")).not.toBeInTheDocument());
    expect(document.body.textContent).not.toContain("sk-new-key");
  });

  it("turns teams may use on with a PUT that keeps the base URL", async () => {
    const fake = fakeApi({ ...listed(), "PUT /api/admin/models/m-opus": { body: { ...modelOpus, teamsMayUse: true } } });
    renderApp("/admin/settings");
    const row = await entry("Claude Opus");
    const toggle = within(row).getByRole("switch", { name: "Teams may use Claude Opus" });
    expect(toggle).not.toBeChecked();
    await userEvent.click(toggle);
    const [put] = fake.find("PUT", "/api/admin/models/m-opus");
    expect(put.headers.get("X-CSRF-Token")).toBe("csrf-123");
    expect(put.body).toEqual({ baseURL: "https://llm.example.com/v1", teamsMayUse: true });
  });

  it("makes another model the default after a confirmation naming the repositories it moves", async () => {
    const confirm = vi.fn(() => true);
    vi.stubGlobal("confirm", confirm);
    const fake = fakeApi({
      ...routes(operator),
      "GET /api/admin/models": { body: models },
      "POST /api/admin/models/m-opus/default": {
        body: { ...models, models: [{ ...modelSonnet, default: false }, { ...modelOpus, default: true }] },
      },
    });
    renderApp("/admin/settings");
    const row = await entry("Claude Opus");
    await userEvent.click(within(row).getByRole("button", { name: "Set Claude Opus as default" }));
    expect(confirm).toHaveBeenCalledWith(expect.stringContaining("Claude Opus"));
    expect(confirm).toHaveBeenCalledWith(expect.stringContaining("3 repositories"));
    expect(fake.find("POST", "/api/admin/models/m-opus/default")[0].headers.get("X-CSRF-Token")).toBe("csrf-123");
    // The answer is the new list: the mark moved.
    await vi.waitFor(async () => expect(within(await entry("Claude Opus")).getByText("default")).toBeInTheDocument());
    expect(within(await entry("Claude Sonnet")).queryByText("default")).not.toBeInTheDocument();
  });

  it("changes nothing when the default's confirmation is refused", async () => {
    vi.stubGlobal("confirm", vi.fn(() => false));
    const fake = fakeApi(listed());
    renderApp("/admin/settings");
    const row = await entry("Claude Opus");
    await userEvent.click(within(row).getByRole("button", { name: "Set Claude Opus as default" }));
    expect(fake.find("POST", "/api/admin/models/m-opus/default")).toHaveLength(0);
  });

  it("offers neither set-as-default nor removal on the default entry", async () => {
    fakeApi(listed());
    renderApp("/admin/settings");
    const row = await entry("Claude Sonnet");
    expect(within(row).queryByRole("button", { name: /as default/ })).not.toBeInTheDocument();
    expect(within(row).queryByRole("button", { name: /^Remove/ })).not.toBeInTheDocument();
    expect(within(row).getByRole("button", { name: "Edit Claude Sonnet" })).toBeInTheDocument();
  });

  it("removes a model after a confirmation that names the chains it leaves, and reads the list again", async () => {
    const confirm = vi.fn(() => true);
    vi.stubGlobal("confirm", confirm);
    let removed = false;
    const fake = fakeApi({
      ...routes(operator),
      "GET /api/admin/models": () => ({ body: removed ? { ...models, models: [modelSonnet] } : models }),
      "DELETE /api/admin/models/m-opus": () => {
        removed = true;
        return { status: 204 };
      },
    });
    renderApp("/admin/settings");
    const row = await entry("Claude Opus");
    await userEvent.click(within(row).getByRole("button", { name: "Remove Claude Opus" }));
    expect(confirm).toHaveBeenCalledWith(
      "Remove Claude Opus from the list? Its key is deleted. These chains name it and will skip it: main/acme/payments.",
    );
    expect(fake.find("DELETE", "/api/admin/models/m-opus")[0].headers.get("X-CSRF-Token")).toBe("csrf-123");
    await vi.waitFor(() => expect(screen.queryByRole("group", { name: "Claude Opus" })).not.toBeInTheDocument());
  });

  it("names no chains in the confirmation of an entry no chain names", async () => {
    const confirm = vi.fn(() => false);
    vi.stubGlobal("confirm", confirm);
    fakeApi(listed({ models: [modelSonnet, { ...modelOpus, usedIn: [] }] }));
    renderApp("/admin/settings");
    await userEvent.click(within(await entry("Claude Opus")).getByRole("button", { name: "Remove Claude Opus" }));
    expect(confirm).toHaveBeenCalledWith("Remove Claude Opus from the list? Its key is deleted.");
  });

  it("shows the server's text when a removal is refused", async () => {
    vi.stubGlobal("confirm", vi.fn(() => true));
    fakeApi({ ...listed(), "DELETE /api/admin/models/m-opus": { status: 500, body: { error: "could not save" } } });
    renderApp("/admin/settings");
    const row = await entry("Claude Opus");
    await userEvent.click(within(row).getByRole("button", { name: "Remove Claude Opus" }));
    expect(await within(row).findByRole("alert")).toHaveTextContent("could not save");
  });

  it("gives the config file's entry no controls, and says where to change it", async () => {
    const config: ModelAdminView = { ...modelSonnet, id: "config", source: "config", default: true, name: "anthropic/claude-sonnet" };
    fakeApi(listed({ canSetDefault: false, models: [config, { ...modelOpus, default: false }] }));
    renderApp("/admin/settings");
    const row = await entry("anthropic/claude-sonnet");
    expect(row).toHaveTextContent("from the config file");
    expect(within(row).queryByRole("button")).not.toBeInTheDocument();
    expect(within(row).queryByRole("switch")).not.toBeInTheDocument();
    // The config's llm: block is the default, but entries can still be
    // added, changed and removed.
    expect(screen.getByText(/llm: block/)).toBeInTheDocument();
    const other = await entry("Claude Opus");
    expect(within(other).queryByRole("button", { name: /as default/ })).not.toBeInTheDocument();
    expect(within(other).getByRole("button", { name: "Edit Claude Opus" })).toBeInTheDocument();
    expect(within(other).getByRole("button", { name: "Remove Claude Opus" })).toBeInTheDocument();
    expect(screen.getByRole("form", { name: "Add a model" })).toBeInTheDocument();
  });

  it("shows a banner and the mark while an entry has one, and clears it", async () => {
    const until = "2026-11-21T14:00:00Z";
    const marked = { ...modelOpus, mark: { class: "quota" as const, until, setAt: until, live: true } };
    let cleared = false;
    const fake = fakeApi({
      ...routes(operator),
      "GET /api/admin/models": () => ({ body: { ...models, models: [modelSonnet, cleared ? modelOpus : marked] } }),
      "DELETE /api/admin/models/m-opus/mark": () => {
        cleared = true;
        return { status: 204 };
      },
    });
    renderApp("/admin/settings");
    const banner = await screen.findByRole("note", { name: "Marked entries" });
    expect(banner).toHaveTextContent("Claude Opus: out of quota until 2026-11-21 14:00 UTC");
    const row = await entry("Claude Opus");
    expect(row).toHaveTextContent("Marked: out of quota until 2026-11-21 14:00 UTC");
    expect(within(await entry("Claude Sonnet")).queryByRole("button", { name: /Clear mark/ })).not.toBeInTheDocument();
    await userEvent.click(within(row).getByRole("button", { name: "Clear mark of Claude Opus" }));
    expect(fake.find("DELETE", "/api/admin/models/m-opus/mark")[0].headers.get("X-CSRF-Token")).toBe("csrf-123");
    await vi.waitFor(() => expect(screen.queryByRole("note", { name: "Marked entries" })).not.toBeInTheDocument());
  });

  it("says so when the list is empty", async () => {
    fakeApi(listed({ models: [], followingDefault: 0 }));
    renderApp("/admin/settings");
    expect(await screen.findByText(/No model yet/)).toBeInTheDocument();
  });

  it("shows the error when the list can't be read", async () => {
    fakeApi({ ...routes(operator), "GET /api/admin/models": { status: 500, body: { error: "boom" } } });
    renderApp("/admin/settings");
    expect(await screen.findByText("boom")).toBeInTheDocument();
  });
});

describe("AdminSettings global chain", () => {
  const chain: ChainView = { ...chainGlobal, entries: [entryConfig, entrySonnet], choices: [choice(entrySonnet), choice(entryOpus)] };

  it("shows the global chain with the config file's entry in front", async () => {
    fakeApi({ ...routes(operator), "GET /api/admin/chain": { body: chain } });
    renderApp("/admin/settings");
    const card = await screen.findByRole("region", { name: "Global chain" });
    const list = await within(card).findByRole("list", { name: "Model chain" });
    expect(within(list).getAllByRole("listitem").map((r) => r.querySelector("b")?.textContent)).toEqual(["openai/gpt-5", "Claude Sonnet"]);
  });

  it("saves the chain with a PUT of its entries, and shows the answer", async () => {
    const saved: ChainView = { ...chain, entries: [entryConfig, { ...entrySonnet, position: 2 }, { ...entryOpus, position: 3 }] };
    const fake = fakeApi({ ...routes(operator), "GET /api/admin/chain": { body: chain }, "PUT /api/admin/chain": { body: saved } });
    renderApp("/admin/settings");
    const card = await screen.findByRole("region", { name: "Global chain" });
    await userEvent.selectOptions(await within(card).findByRole("combobox", { name: "Entry to add" }), "Claude Opus (global)");
    await userEvent.click(within(card).getByRole("button", { name: "Add" }));
    await userEvent.click(within(card).getByRole("button", { name: "Save chain" }));
    const [put] = fake.find("PUT", "/api/admin/chain");
    expect(put.headers.get("X-CSRF-Token")).toBe("csrf-123");
    expect(put.body).toEqual({
      entries: [
        { source: "global", id: "m-sonnet" },
        { source: "global", id: "m-opus" },
      ],
    });
    expect(await within(card).findByRole("status")).toHaveTextContent("Saved.");
  });

  it("shows the server's text when a save is refused", async () => {
    fakeApi({
      ...routes(operator),
      "GET /api/admin/chain": { body: chain },
      "PUT /api/admin/chain": { status: 400, body: { error: "the chain has more than 5 entries" } },
    });
    renderApp("/admin/settings");
    const card = await screen.findByRole("region", { name: "Global chain" });
    await userEvent.click(await within(card).findByRole("button", { name: "Save chain" }));
    expect(await within(card).findByRole("alert")).toHaveTextContent("the chain has more than 5 entries");
  });

  it("clears a mark from the chain with the admin route", async () => {
    const marked = { ...entrySonnet, state: "exhausted" as const, class: "quota" as const, until: "2026-11-21T14:00:00Z", canClear: true };
    const fake = fakeApi({
      ...routes(operator),
      "GET /api/admin/chain": { body: { ...chain, entries: [entryConfig, marked] } },
      "DELETE /api/admin/models/m-sonnet/mark": { status: 204 },
    });
    renderApp("/admin/settings");
    const card = await screen.findByRole("region", { name: "Global chain" });
    await userEvent.click(await within(card).findByRole("button", { name: "Clear mark of Claude Sonnet" }));
    expect(fake.find("DELETE", "/api/admin/models/m-sonnet/mark")).toHaveLength(1);
  });
});

describe("AdminSettings access", () => {
  it("isn't shown to a viewer", async () => {
    const fake = fakeApi({ "GET /api/me": { body: viewer } });
    renderApp("/admin/settings");
    expect(await screen.findByRole("heading", { name: "Not found" })).toBeInTheDocument();
    expect(fake.find("GET", "/api/admin/defaults")).toHaveLength(0);
    expect(fake.find("GET", "/api/admin/models")).toHaveLength(0);
  });
});

describe("AdminSettings global chain B", () => {
  const panel: ChainView = { ...chainGlobal, entries: [], choices: [choice(entrySonnet), choice(entryOpus)] };

  it("saves chain B with a PUT, and an empty one is allowed", async () => {
    const saved: ChainView = { ...panel, entries: [{ ...entryOpus, position: 1 }] };
    const fake = fakeApi({
      ...routes(operator),
      "GET /api/admin/panel-chain": { body: panel },
      "PUT /api/admin/panel-chain": { body: saved },
    });
    renderApp("/admin/settings");
    const card = await screen.findByRole("region", { name: "Global chain B (review panel)" });
    await userEvent.selectOptions(await within(card).findByRole("combobox", { name: "Entry to add" }), `global:${entryOpus.id}`);
    await userEvent.click(within(card).getByRole("button", { name: "Add" }));
    await userEvent.click(within(card).getByRole("button", { name: "Save chain" }));
    expect(fake.find("PUT", "/api/admin/panel-chain")[0].body).toEqual({ entries: [{ source: "global", id: entryOpus.id }] });
    expect(await within(card).findByText("Saved.")).toBeInTheDocument();
  });

  it("shows the message of a refused save", async () => {
    fakeApi({
      ...routes(operator),
      "GET /api/admin/panel-chain": { body: panel },
      "PUT /api/admin/panel-chain": { status: 400, body: { error: "no review image supports a review panel yet" } },
    });
    renderApp("/admin/settings");
    const card = await screen.findByRole("region", { name: "Global chain B (review panel)" });
    await userEvent.selectOptions(await within(card).findByRole("combobox", { name: "Entry to add" }), `global:${entryOpus.id}`);
    await userEvent.click(within(card).getByRole("button", { name: "Add" }));
    await userEvent.click(within(card).getByRole("button", { name: "Save chain" }));
    expect(await within(card).findByText(/no review image supports a review panel yet/)).toBeInTheDocument();
  });
});
