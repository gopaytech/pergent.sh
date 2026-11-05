import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { fakeApi } from "../test/fakeApi";
import { platformAdmin, viewer, librarySkills, repoDetail, repoPayments, repoWidgets, reviewBlocking } from "../test/fixtures";
import { renderApp } from "../test/render";

afterEach(() => vi.unstubAllGlobals());

const payments = repoDetail(repoPayments);
const widgets = repoDetail(repoWidgets);

const noInputs = { branch: "main", skills: [], detected: [], instructions: "" };

const base = {
  "GET /api/me": { body: platformAdmin },
  "GET /api/repositories/main/acme/payments": { body: payments },
  "GET /api/repositories/main/acme/widgets": { body: widgets },
  "GET /api/skills": { body: librarySkills },
  "GET /api/repositories/main/acme/payments/inputs": { body: noInputs },
  "GET /api/repositories/main/acme/widgets/inputs": { body: noInputs },
  "GET /api/reviews?project=acme%2Fpayments&days=90": { body: { items: [reviewBlocking], nextCursor: "" } },
};

describe("RepositoryDetail", () => {
  it("shows the repository's reviews first", async () => {
    renderAt("/repos/main/acme/payments", base);
    expect(await screen.findByRole("heading", { name: "acme/payments" })).toBeInTheDocument();
    expect(screen.getByText(/GitLab · main · switched on in the dashboard/)).toBeInTheDocument();
    const tabs = screen.getByRole("tablist");
    for (const name of ["Reviews", "Settings", "Skills & instructions", "Setup checks (1)"]) {
      expect(within(tabs).getByRole("tab", { name })).toBeInTheDocument();
    }
    expect(within(tabs).getByRole("tab", { name: "Reviews" })).toHaveAttribute("aria-selected", "true");
    expect(await screen.findByRole("link", { name: /acme\/payments !77/ })).toHaveAttribute(
      "href",
      "/reviews/main/acme/payments/77",
    );
  });

  it("shows Settings to a maintainer of the repository, whoever they are elsewhere", async () => {
    renderAt("/repos/main/acme/payments", {
      ...base,
      "GET /api/me": { body: viewer },
      "GET /api/repositories/main/acme/payments": { body: repoDetail(repoPayments, "maintainer") },
    });
    expect(await screen.findByRole("tab", { name: "Settings" })).toBeInTheDocument();
  });

  it("hides Settings from a viewer of the repository, even an admin elsewhere", async () => {
    renderAt("/repos/main/acme/payments?tab=settings", {
      ...base,
      "GET /api/me": { body: platformAdmin },
      "GET /api/repositories/main/acme/payments": { body: repoDetail(repoPayments, "viewer") },
    });
    await screen.findByRole("heading", { name: "acme/payments" });
    expect(screen.queryByRole("tab", { name: "Settings" })).not.toBeInTheDocument();
    // ?tab=settings falls back to Reviews.
    expect(screen.getByRole("tab", { name: "Reviews" })).toHaveAttribute("aria-selected", "true");
    expect(screen.queryByRole("switch")).not.toBeInTheDocument();
  });

  it("shows each setting as overridden here or default", async () => {
    renderAt("/repos/main/acme/payments?tab=settings", base);
    const approve = (await screen.findByRole("switch", { name: "Approve clean reviews" })).closest(".set") as HTMLElement;
    expect(within(approve).getByText("overridden here")).toHaveClass("badge");
    expect(within(approve).getByRole("switch")).toBeChecked();
    const drafts = screen.getByRole("switch", { name: "Review drafts" }).closest(".set") as HTMLElement;
    expect(within(drafts).getByText("default")).toHaveClass("badge", "cfg");
    expect(screen.getByRole("combobox", { name: "Status when a review fails" })).toHaveValue("failed");
    expect(screen.getByRole("textbox", { name: "Timeout (seconds)" })).toHaveValue("600");
    const memory = screen.getByRole("textbox", { name: "Memory limit" });
    expect(memory).toHaveValue("6Gi");
    expect(within(memory.closest(".set") as HTMLElement).getByText("overridden here")).toBeInTheDocument();
    // skills null: the default list, no switches to choose with.
    expect(await screen.findByRole("radio", { name: "Use the default skills (code-review)" })).toBeChecked();
    expect(screen.getByRole("radio", { name: "Choose skills" })).not.toBeChecked();
    expect(screen.queryByRole("switch", { name: "Skill code-review" })).not.toBeInTheDocument();
    expect(screen.queryByText("managed by config")).not.toBeInTheDocument();
  });

  it("disables a config-managed repository's settings", async () => {
    const fake = fakeApi(base);
    renderApp("/repos/main/acme/widgets?tab=settings");
    expect(await screen.findAllByText("managed by config")).not.toHaveLength(0);
    // A config repository can't choose, but its skills are shown.
    await screen.findByRole("switch", { name: "Skill code-review" });
    for (const el of [
      screen.getByRole("switch", { name: "Approve clean reviews" }),
      screen.getByRole("switch", { name: "Review drafts" }),
      screen.getByRole("combobox", { name: "Status when a review fails" }),
      screen.getByRole("textbox", { name: "Timeout (seconds)" }),
      screen.getByRole("textbox", { name: "Max steps" }),
      screen.getByRole("textbox", { name: "Memory limit" }),
      screen.getByRole("switch", { name: "Skill code-review" }),
      screen.getByRole("radio", { name: "Choose skills" }),
      screen.getByRole("radio", { name: /Use the default skills/ }),
    ]) {
      expect(el).toBeDisabled();
    }
    expect(screen.queryByRole("button", { name: "Save" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Reset to defaults" })).not.toBeInTheDocument();
    expect(fake.calls.filter((c) => c.method !== "GET")).toHaveLength(0);
  });

  it("saves only what changed, with the CSRF token", async () => {
    const saved = {
      ...payments,
      settings: {
        ...payments.settings,
        approve: { value: false, overridden: true },
        jobTimeoutSeconds: { value: 900, overridden: true },
        skills: { value: ["code-review", "security-review"], overridden: true },
      },
    };
    const fake = fakeApi({ ...base, "PUT /api/repositories/main/acme/payments/settings": { body: saved } });
    renderApp("/repos/main/acme/payments?tab=settings");
    await userEvent.click(await screen.findByRole("switch", { name: "Approve clean reviews" }));
    const timeout = screen.getByRole("textbox", { name: "Timeout (seconds)" });
    await userEvent.clear(timeout);
    await userEvent.type(timeout, "900");
    await userEvent.click(await screen.findByRole("radio", { name: "Choose skills" }));
    await userEvent.click(await screen.findByRole("switch", { name: "Skill security-review" }));
    await userEvent.click(screen.getByRole("button", { name: "Save" }));

    const [put] = fake.find("PUT", "/api/repositories/main/acme/payments/settings");
    expect(put.headers.get("X-CSRF-Token")).toBe("csrf-123");
    expect(put.body).toEqual({ approve: false, jobTimeoutSeconds: 900, skills: ["code-review", "security-review"] });
    expect(await screen.findByText("Saved.")).toBeInTheDocument();
    const timeoutRow = screen.getByRole("textbox", { name: "Timeout (seconds)" }).closest(".set") as HTMLElement;
    expect(within(timeoutRow).getByText("overridden here")).toBeInTheDocument();
  });

  it("shows Max steps with its help text and whether it is overridden", async () => {
    fakeApi(base);
    renderApp("/repos/main/acme/payments?tab=settings");
    const steps = await screen.findByRole("textbox", { name: "Max steps" });
    expect(steps).toHaveValue("40");
    const row = steps.closest(".set") as HTMLElement;
    expect(within(row).getByText("default")).toHaveClass("badge", "cfg");
    expect(
      within(row).getByText("Steps each opencode run may take before pergent stops it; each step can call several tools."),
    ).toBeInTheDocument();
  });

  it("saves a changed Max steps and shows it overridden", async () => {
    const saved = { ...payments, settings: { ...payments.settings, maxTurns: { value: 120, overridden: true } } };
    const fake = fakeApi({ ...base, "PUT /api/repositories/main/acme/payments/settings": { body: saved } });
    renderApp("/repos/main/acme/payments?tab=settings");
    const steps = await screen.findByRole("textbox", { name: "Max steps" });
    await userEvent.clear(steps);
    await userEvent.type(steps, "120");
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(fake.find("PUT", "/api/repositories/main/acme/payments/settings")[0].body).toEqual({ maxTurns: 120 });
    expect(await screen.findByText("Saved.")).toBeInTheDocument();
    const row = screen.getByRole("textbox", { name: "Max steps" }).closest(".set") as HTMLElement;
    expect(within(row).getByText("overridden here")).toBeInTheDocument();
  });

  it("sends an emptied Max steps as null, the default", async () => {
    const fake = fakeApi({ ...base, "PUT /api/repositories/main/acme/payments/settings": { body: payments } });
    renderApp("/repos/main/acme/payments?tab=settings");
    await userEvent.clear(await screen.findByRole("textbox", { name: "Max steps" }));
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(fake.find("PUT", "/api/repositories/main/acme/payments/settings")[0].body).toEqual({ maxTurns: null });
  });

  it.each(["4", "201", "abc", "40.5"])("refuses Max steps %s without asking the server", async (text) => {
    const fake = fakeApi(base);
    renderApp("/repos/main/acme/payments?tab=settings");
    const steps = await screen.findByRole("textbox", { name: "Max steps" });
    await userEvent.clear(steps);
    await userEvent.type(steps, text);
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(await screen.findByText("Max steps is a whole number from 5 to 200.")).toBeInTheDocument();
    expect(fake.calls.filter((c) => c.method === "PUT")).toHaveLength(0);
  });

  const timeoutProblem = "The timeout is a whole number of seconds, from 60 to 7200.";

  it.each(["abc", "59", "7201", "90.5"])("refuses Timeout %s without asking the server", async (text) => {
    const fake = fakeApi(base);
    renderApp("/repos/main/acme/payments?tab=settings");
    const timeout = await screen.findByRole("textbox", { name: "Timeout (seconds)" });
    await userEvent.clear(timeout);
    await userEvent.type(timeout, text);
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(await screen.findByText(timeoutProblem)).toBeInTheDocument();
    expect(fake.calls.filter((c) => c.method === "PUT")).toHaveLength(0);
  });

  it("sends an emptied Timeout as null, the default", async () => {
    const fake = fakeApi({ ...base, "PUT /api/repositories/main/acme/payments/settings": { body: payments } });
    renderApp("/repos/main/acme/payments?tab=settings");
    await userEvent.clear(await screen.findByRole("textbox", { name: "Timeout (seconds)" }));
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(fake.find("PUT", "/api/repositories/main/acme/payments/settings")[0].body).toEqual({ jobTimeoutSeconds: null });
  });

  it("shows the re-run caps, editable by an admin of the repository", async () => {
    fakeApi(base);
    renderApp("/repos/main/acme/payments?tab=settings");
    const perMR = await screen.findByRole("spinbutton", { name: "Re-run cap per MR/PR (per day)" });
    const perRepo = screen.getByRole("spinbutton", { name: "Re-run cap per repository (per day)" });
    expect(perMR).toHaveValue(10);
    expect(perRepo).toHaveValue(50);
    expect(within(perMR.closest(".set") as HTMLElement).getByText("default")).toBeInTheDocument();
    expect(perMR).not.toHaveAttribute("readonly");
    expect(perMR).toBeEnabled();
    expect(perRepo).not.toHaveAttribute("readonly");
    expect(screen.queryByText(/Only admins of this repository change its re-run caps/)).not.toBeInTheDocument();
  });

  it("saves a changed re-run cap and leaves the other alone", async () => {
    const saved = {
      ...payments,
      settings: { ...payments.settings, rerunLimitPerRepo: { value: 200, overridden: true } },
    };
    const fake = fakeApi({ ...base, "PUT /api/repositories/main/acme/payments/settings": { body: saved } });
    renderApp("/repos/main/acme/payments?tab=settings");
    const perRepo = await screen.findByRole("spinbutton", { name: "Re-run cap per repository (per day)" });
    await userEvent.clear(perRepo);
    await userEvent.type(perRepo, "200");
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    const [put] = fake.find("PUT", "/api/repositories/main/acme/payments/settings");
    expect(put.headers.get("X-CSRF-Token")).toBe("csrf-123");
    expect(put.body).toEqual({ rerunLimitPerRepo: 200 });
    expect(await screen.findByText("Saved.")).toBeInTheDocument();
    const row = screen.getByRole("spinbutton", { name: "Re-run cap per repository (per day)" }).closest(".set") as HTMLElement;
    expect(within(row).getByText("overridden here")).toBeInTheDocument();
    expect(screen.getByRole("spinbutton", { name: "Re-run cap per repository (per day)" })).toHaveValue(200);
  });

  it("sends an emptied re-run cap as null, the default", async () => {
    const fake = fakeApi({ ...base, "PUT /api/repositories/main/acme/payments/settings": { body: payments } });
    renderApp("/repos/main/acme/payments?tab=settings");
    await userEvent.clear(await screen.findByRole("spinbutton", { name: "Re-run cap per MR/PR (per day)" }));
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(fake.find("PUT", "/api/repositories/main/acme/payments/settings")[0].body).toEqual({ rerunLimitPerMR: null });
  });

  it.each(["0", "1001", "2.5"])("refuses the re-run cap %s without asking the server", async (value) => {
    const fake = fakeApi(base);
    renderApp("/repos/main/acme/payments?tab=settings");
    const perMR = await screen.findByRole("spinbutton", { name: "Re-run cap per MR/PR (per day)" });
    await userEvent.clear(perMR);
    await userEvent.type(perMR, value);
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(screen.getByText("The re-run caps are whole numbers from 1 to 1000.")).toBeInTheDocument();
    expect(fake.calls.filter((c) => c.method === "PUT")).toHaveLength(0);
  });

  it("shows Inline findings after Review drafts and sends only that key when it is switched off", async () => {
    const fake = fakeApi({
      ...base,
      "PUT /api/repositories/main/acme/payments/settings": { body: payments },
    });
    renderApp("/repos/main/acme/payments?tab=settings");
    const toggle = await screen.findByRole("switch", { name: "Inline findings" });
    expect(toggle).toBeChecked();
    expect(
      screen.getByText(
        "Post each finding as a comment on its lines, with a one-click fix when it is small. Off keeps every finding in the review comment.",
      ),
    ).toBeInTheDocument();
    const switches = screen.getAllByRole("switch").map((el) => el.getAttribute("aria-label") ?? el.textContent);
    expect(switches.indexOf("Inline findings")).toBe(switches.indexOf("Review drafts") + 1);
    await userEvent.click(toggle);
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(fake.find("PUT", "/api/repositories/main/acme/payments/settings")[0].body).toEqual({ inlineFindings: false });
  });

  it("shows the commands switch after Inline findings and sends only that key when it is switched off", async () => {
    const fake = fakeApi({
      ...base,
      "PUT /api/repositories/main/acme/payments/settings": { body: payments },
    });
    renderApp("/repos/main/acme/payments?tab=settings");
    const toggle = await screen.findByRole("switch", { name: "Only the author and maintainers use commands" });
    expect(toggle).toBeChecked();
    expect(
      screen.getByText(
        "When on, only the author of a change and the repository's maintainers and owners can use pergent commands on it. When off, anyone with write access can.",
      ),
    ).toBeInTheDocument();
    const switches = screen.getAllByRole("switch").map((el) => el.getAttribute("aria-label") ?? el.textContent);
    expect(switches.indexOf("Only the author and maintainers use commands")).toBe(switches.indexOf("Inline findings") + 1);
    await userEvent.click(toggle);
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(fake.find("PUT", "/api/repositories/main/acme/payments/settings")[0].body).toEqual({
      commandsAuthorAndMaintainersOnly: false,
    });
  });

  it("shows the incremental reviews switch after the commands switch and sends only that key when it is switched off", async () => {
    const fake = fakeApi({
      ...base,
      "PUT /api/repositories/main/acme/payments/settings": { body: payments },
    });
    renderApp("/repos/main/acme/payments?tab=settings");
    const toggle = await screen.findByRole("switch", { name: "Incremental reviews" });
    expect(toggle).toBeChecked();
    expect(screen.getByText("After a push, review only the files changed since the last review.")).toBeInTheDocument();
    const switches = screen.getAllByRole("switch").map((el) => el.getAttribute("aria-label") ?? el.textContent);
    expect(switches.indexOf("Incremental reviews")).toBe(switches.indexOf("Only the author and maintainers use commands") + 1);
    await userEvent.click(toggle);
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(fake.find("PUT", "/api/repositories/main/acme/payments/settings")[0].body).toEqual({
      incrementalReviews: false,
    });
  });

  it("shows the learn from dismissals switch after the incremental reviews switch and sends only that key when it is switched off", async () => {
    const fake = fakeApi({
      ...base,
      "PUT /api/repositories/main/acme/payments/settings": { body: payments },
    });
    renderApp("/repos/main/acme/payments?tab=settings");
    const toggle = await screen.findByRole("switch", { name: "Learn from dismissals" });
    expect(toggle).toBeChecked();
    expect(screen.getByText("Later reviews are told what people with write access rejected, and why.")).toBeInTheDocument();
    const switches = screen.getAllByRole("switch").map((el) => el.getAttribute("aria-label") ?? el.textContent);
    expect(switches.indexOf("Learn from dismissals")).toBe(switches.indexOf("Incremental reviews") + 1);
    await userEvent.click(toggle);
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(fake.find("PUT", "/api/repositories/main/acme/payments/settings")[0].body).toEqual({
      learnFromDismissals: false,
    });
  });

  it("shows a maintainer the re-run caps read-only and never sends them", async () => {
    const asMaintainer = repoDetail(repoPayments, "maintainer");
    const fake = fakeApi({
      ...base,
      "GET /api/repositories/main/acme/payments": { body: asMaintainer },
      "PUT /api/repositories/main/acme/payments/settings": { body: asMaintainer },
    });
    renderApp("/repos/main/acme/payments?tab=settings");
    const perMR = await screen.findByRole("spinbutton", { name: "Re-run cap per MR/PR (per day)" });
    const perRepo = screen.getByRole("spinbutton", { name: "Re-run cap per repository (per day)" });
    expect(perMR).toHaveAttribute("readonly");
    expect(perRepo).toHaveAttribute("readonly");
    expect(perMR).toHaveValue(10);
    expect(screen.getByText("Only admins of this repository change its re-run caps.")).toBeInTheDocument();
    // Typing does nothing to a read-only input.
    await userEvent.type(perMR, "5");
    expect(perMR).toHaveValue(10);
    await userEvent.click(screen.getByRole("switch", { name: "Review drafts" }));
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(fake.find("PUT", "/api/repositories/main/acme/payments/settings")[0].body).toEqual({ reviewDrafts: true });
  });

  it("says in the reset confirmation that a maintainer's reset leaves the re-run caps", async () => {
    const asMaintainer = repoDetail(repoPayments, "maintainer");
    fakeApi({ ...base, "GET /api/repositories/main/acme/payments": { body: asMaintainer } });
    const confirm = vi.spyOn(window, "confirm").mockReturnValue(false);
    renderApp("/repos/main/acme/payments?tab=settings");
    await userEvent.click(await screen.findByRole("button", { name: "Reset to defaults" }));
    expect(confirm).toHaveBeenCalledWith(expect.stringContaining("The re-run caps stay as they are"));
    confirm.mockRestore();
  });

  it("doesn't say so to an admin, whose reset covers the caps", async () => {
    fakeApi(base);
    const confirm = vi.spyOn(window, "confirm").mockReturnValue(false);
    renderApp("/repos/main/acme/payments?tab=settings");
    await userEvent.click(await screen.findByRole("button", { name: "Reset to defaults" }));
    expect(confirm).toHaveBeenCalledWith("Put every setting of this repository back to its default?");
    confirm.mockRestore();
  });

  it("disables the re-run caps of a config-managed repository", async () => {
    fakeApi(base);
    renderApp("/repos/main/acme/widgets?tab=settings");
    await screen.findAllByText("managed by config");
    expect(screen.getByRole("spinbutton", { name: "Re-run cap per MR/PR (per day)" })).toBeDisabled();
    expect(screen.getByRole("spinbutton", { name: "Re-run cap per repository (per day)" })).toBeDisabled();
  });

  it("shows the server's error when a value is refused", async () => {
    fakeApi({
      ...base,
      "PUT /api/repositories/main/acme/payments/settings": { status: 400, body: { error: "jobMemory must be at least 256Mi" } },
    });
    renderApp("/repos/main/acme/payments?tab=settings");
    const memory = await screen.findByRole("textbox", { name: "Memory limit" });
    await userEvent.clear(memory);
    await userEvent.type(memory, "1Mi");
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("jobMemory must be at least 256Mi");
  });

  it("resets every setting to its default", async () => {
    vi.stubGlobal("confirm", vi.fn(() => true));
    const fake = fakeApi({
      ...base,
      "POST /api/repositories/main/acme/payments/settings/reset": { body: payments },
    });
    renderApp("/repos/main/acme/payments?tab=settings");
    await userEvent.click(await screen.findByRole("button", { name: "Reset to defaults" }));
    const [post] = fake.find("POST", "/api/repositories/main/acme/payments/settings/reset");
    expect(post.headers.get("X-CSRF-Token")).toBe("csrf-123");
  });

  it("offers the default skills or a choice, with a running total", async () => {
    const fake = fakeApi({
      ...base,
      "PUT /api/repositories/main/acme/payments/settings": {
        body: {
          ...payments,
          settings: { ...payments.settings, skills: { value: ["security-review"], overridden: true } },
        },
      },
    });
    renderApp("/repos/main/acme/payments?tab=settings");
    const skills = await screen.findByRole("region", { name: "Skills used for this repository" });
    const defaultsRadio = within(skills).getByRole("radio", { name: "Use the default skills (code-review)" });
    expect(defaultsRadio).toBeChecked();
    expect(within(skills).getByText("Total 52 B of 900 KiB")).toBeInTheDocument();

    await userEvent.click(within(skills).getByRole("radio", { name: "Choose skills" }));
    // Choosing starts from the default list.
    expect(within(skills).getByRole("switch", { name: "Skill code-review" })).toBeChecked();
    expect(within(skills).getByRole("switch", { name: "Skill security-review" })).not.toBeChecked();
    await userEvent.click(within(skills).getByRole("switch", { name: "Skill code-review" }));
    await userEvent.click(within(skills).getByRole("switch", { name: "Skill security-review" }));
    expect(within(skills).getByText("Total 61 B of 900 KiB")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(fake.find("PUT", "/api/repositories/main/acme/payments/settings")[0].body).toEqual({ skills: ["security-review"] });
    expect(await screen.findByText("Saved.")).toBeInTheDocument();
  });

  it("puts a repository back on the default skills with null", async () => {
    const chosen = {
      ...payments,
      settings: { ...payments.settings, skills: { value: ["security-review"], overridden: true } },
    };
    const fake = fakeApi({
      ...base,
      "GET /api/repositories/main/acme/payments": { body: chosen },
      "PUT /api/repositories/main/acme/payments/settings": { body: payments },
    });
    renderApp("/repos/main/acme/payments?tab=settings");
    expect(await screen.findByRole("radio", { name: "Choose skills" })).toBeChecked();
    await userEvent.click(screen.getByRole("radio", { name: /Use the default skills/ }));
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(fake.find("PUT", "/api/repositories/main/acme/payments/settings")[0].body).toEqual({ skills: null });
  });

  it("says when the default skill list is empty", async () => {
    fakeApi({
      ...base,
      "GET /api/skills": { body: librarySkills.map((s) => ({ ...s, inDefaults: false })) },
    });
    renderApp("/repos/main/acme/payments?tab=settings");
    expect(await screen.findByRole("radio", { name: "Use the default skills (none)" })).toBeChecked();
    expect(screen.getByText(/no default skills/i)).toBeInTheDocument();
  });

  it("shows the detected skills with switches, off at first, and .pergent/skills as always on", async () => {
    const fake = fakeApi({
      ...base,
      "GET /api/repositories/main/acme/payments/inputs": { body: detectedInputs },
      "PUT /api/repositories/main/acme/payments/settings": {
        body: { ...payments, settings: { ...payments.settings, repoSkills: { value: ["agent-rules"], overridden: true } } },
      },
    });
    renderApp("/repos/main/acme/payments?tab=settings");
    const section = await screen.findByRole("region", { name: "Skills in the repository" });
    const agent = await within(section).findByRole("switch", { name: "Repository skill agent-rules" });
    expect(agent).not.toBeChecked();
    expect(within(section).getByRole("switch", { name: "Repository skill lint" })).not.toBeChecked();
    expect(agent.closest(".skill")).toHaveTextContent(".agents/skills");
    expect(within(section).getByText(".pergent/skills").closest(".skill")).toHaveTextContent("always on");
    expect(section).toHaveTextContent("Only the SKILL.md text of a skill is used");
    // A skill that can't be read says why and can't be turned on.
    const broken = within(section).getByRole("switch", { name: "Repository skill broken" });
    expect(broken).toBeDisabled();
    expect(broken.closest(".skill")).toHaveTextContent("frontmatter does not parse");

    await userEvent.click(agent);
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(fake.find("PUT", "/api/repositories/main/acme/payments/settings")[0].body).toEqual({ repoSkills: ["agent-rules"] });
  });

  it("keeps the skills turned on and shows one the repository no longer has", async () => {
    const fake = fakeApi({
      ...base,
      "GET /api/repositories/main/acme/payments": {
        body: { ...payments, settings: { ...payments.settings, repoSkills: { value: ["lint", "vanished"], overridden: true } } },
      },
      "GET /api/repositories/main/acme/payments/inputs": {
        body: { ...detectedInputs, detected: detectedInputs.detected.map((d) => (d.name === "lint" ? { ...d, on: true } : d)) },
      },
      "PUT /api/repositories/main/acme/payments/settings": { body: payments },
    });
    renderApp("/repos/main/acme/payments?tab=settings");
    expect(await screen.findByRole("switch", { name: "Repository skill lint" })).toBeChecked();
    const vanished = screen.getByRole("switch", { name: "Repository skill vanished" });
    expect(vanished).toBeChecked();
    expect(vanished.closest(".skill")).toHaveTextContent("not found in the repository");
    await userEvent.click(vanished);
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(fake.find("PUT", "/api/repositories/main/acme/payments/settings")[0].body).toEqual({ repoSkills: ["lint"] });
  });

  it("keeps the detected skills out of the save when they can't be read", async () => {
    const fake = fakeApi({
      ...base,
      "GET /api/repositories/main/acme/payments/inputs": {
        status: 502,
        body: { error: "could not read the repository's skills and instructions from the platform, please try again" },
      },
      "PUT /api/repositories/main/acme/payments/settings": { body: payments },
    });
    renderApp("/repos/main/acme/payments?tab=settings");
    const section = await screen.findByRole("region", { name: "Skills in the repository" });
    expect(await within(section).findByText(/could not read the repository's skills/)).toBeInTheDocument();
    await userEvent.click(await screen.findByRole("switch", { name: "Review drafts" }));
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(fake.find("PUT", "/api/repositories/main/acme/payments/settings")[0].body).toEqual({ reviewDrafts: true });
  });

  it("shows the folder, the version and a file link for each skill a review would use", async () => {
    fakeApi({
      ...base,
      "GET /api/repositories/main/acme/payments/inputs": {
        body: {
          branch: "main",
          skills: [
            { name: "code-review", source: "library", folder: "", version: 2, content: "Review.", url: "" },
            {
              name: "agent-rules",
              source: ".claude/skills",
              folder: ".claude/skills",
              version: 0,
              content: "Be <i>strict</i>.",
              url: "https://gitlab.example.com/acme/payments/-/blob/main/.claude/skills/agent-rules/SKILL.md",
            },
            { name: "evil", source: ".pergent/skills", folder: ".pergent/skills", version: 0, content: "x", url: "javascript:alert(1)" },
            { name: "gone", source: "", folder: "", version: 0, content: "", url: "", problem: 'skill "gone" is not in the skill library' },
          ],
          detected: [],
          instructions: "",
        },
      },
    });
    renderApp("/repos/main/acme/payments?tab=inputs");
    const skills = await screen.findByRole("region", { name: "Skills" });
    const lib = within(skills).getByText("code-review").closest(".skill-doc") as HTMLElement;
    expect(lib).toHaveTextContent("library v2");
    expect(within(lib).queryByRole("link")).not.toBeInTheDocument();

    const folder = within(skills).getByText("agent-rules").closest(".skill-doc") as HTMLElement;
    expect(folder).toHaveTextContent(".claude/skills");
    expect(within(folder).getByRole("link", { name: "View file" })).toHaveAttribute(
      "href",
      "https://gitlab.example.com/acme/payments/-/blob/main/.claude/skills/agent-rules/SKILL.md",
    );
    expect(within(folder).getByText("Be <i>strict</i>.").tagName).toBe("PRE");

    // A link that isn't http(s) is never made.
    expect(within(within(skills).getByText("evil").closest(".skill-doc") as HTMLElement).queryByRole("link")).not.toBeInTheDocument();

    const gone = within(skills).getByText("gone").closest(".skill-doc") as HTMLElement;
    expect(gone).toHaveTextContent('skill "gone" is not in the skill library');
  });

  it("shows the resolved skills and instructions", async () => {
    fakeApi({
      ...base,
      "GET /api/repositories/main/acme/payments/inputs": {
        body: {
          branch: "main",
          skills: [
            { name: "code-review", source: "repo", folder: "", version: 0, content: "Repo's own <b>review</b>.", url: "" },
          ],
          detected: [],
          instructions: "Be kind.\nBe brief.",
        },
      },
    });
    renderApp("/repos/main/acme/payments?tab=inputs");
    expect(await screen.findByText(/Read from the default branch/)).toHaveTextContent("main");
    expect(screen.getByText("code-review").closest(".skill")).toHaveTextContent("repo");
    expect(screen.getByText("Repo's own <b>review</b>.").tagName).toBe("PRE");
    expect(screen.getByText(/Be kind\./).tagName).toBe("PRE");
  });

  it("says when the repository isn't set up yet", async () => {
    fakeApi({
      ...base,
      "GET /api/repositories/main/acme/payments/inputs": {
        status: 503,
        body: { error: "pergent.sh hasn't set this repository up yet; try again in a minute" },
      },
    });
    renderApp("/repos/main/acme/payments?tab=inputs");
    expect(await screen.findByText(/hasn't set this repository up yet/)).toBeInTheDocument();
  });

  it("lists the setup checks", async () => {
    renderAt("/repos/main/acme/payments?tab=setup", base);
    expect(await screen.findByText(/One approval is enough/)).toBeInTheDocument();
  });

  it("shows not found for a repository the user can't see", async () => {
    renderAt("/repos/main/acme/secret", base);
    expect(await screen.findByRole("heading", { name: "Not found" })).toBeInTheDocument();
  });

  it("refreshes the page after saving when the address spells the project differently", async () => {
    const saved = { ...payments, settings: { ...payments.settings, reviewDrafts: { value: true, overridden: true } } };
    const fake = fakeApi({
      ...base,
      "GET /api/repositories/main/ACME/Payments": { body: payments },
      "PUT /api/repositories/main/acme/payments/settings": { body: saved },
    });
    renderApp("/repos/main/ACME/Payments?tab=settings");
    const drafts = await screen.findByRole("switch", { name: "Review drafts" });
    expect(within(drafts.closest(".set") as HTMLElement).getByText("default")).toBeInTheDocument();
    await userEvent.click(drafts);
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(fake.find("PUT", "/api/repositories/main/acme/payments/settings")).toHaveLength(1);
    const row = (await screen.findByText("Saved.")) && screen.getByRole("switch", { name: "Review drafts" }).closest(".set");
    expect(within(row as HTMLElement).getByText("overridden here")).toBeInTheDocument();
  });

  it("drops skills no longer in the library when saving the skills", async () => {
    const stale = {
      ...payments,
      settings: { ...payments.settings, skills: { value: ["code-review", "old-skill"], overridden: true } },
    };
    const fake = fakeApi({
      ...base,
      "GET /api/repositories/main/acme/payments": { body: stale },
      "PUT /api/repositories/main/acme/payments/settings": { body: payments },
    });
    renderApp("/repos/main/acme/payments?tab=settings");
    expect(await screen.findByText(/old-skill is no longer in the library/)).toBeInTheDocument();
    await userEvent.click(screen.getByRole("switch", { name: "Skill security-review" }));
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    const [put] = fake.find("PUT", "/api/repositories/main/acme/payments/settings");
    expect(put.body).toEqual({ skills: ["code-review", "security-review"] });
  });

  it("retries a write once with a fresh CSRF token after a CSRF 403", async () => {
    let puts = 0;
    let mes = 0;
    const fake = fakeApi({
      ...base,
      "GET /api/me": () => ({ body: mes++ === 0 ? platformAdmin : { ...platformAdmin, csrfToken: "csrf-new" } }),
      "PUT /api/repositories/main/acme/payments/settings": () =>
        puts++ === 0 ? { status: 403, body: { error: "missing or wrong CSRF token", code: "csrf" } } : { body: payments },
    });
    renderApp("/repos/main/acme/payments?tab=settings");
    await userEvent.click(await screen.findByRole("switch", { name: "Review drafts" }));
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(await screen.findByText("Saved.")).toBeInTheDocument();
    const calls = fake.find("PUT", "/api/repositories/main/acme/payments/settings");
    expect(calls.map((c) => c.headers.get("X-CSRF-Token"))).toEqual(["csrf-123", "csrf-new"]);
  });

  it("doesn't retry a 403 that isn't about the CSRF token", async () => {
    const fake = fakeApi({
      ...base,
      "PUT /api/repositories/main/acme/payments/settings": { status: 403, body: { error: "only admins can do this" } },
    });
    renderApp("/repos/main/acme/payments?tab=settings");
    await userEvent.click(await screen.findByRole("switch", { name: "Review drafts" }));
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("only admins can do this");
    expect(fake.find("PUT", "/api/repositories/main/acme/payments/settings")).toHaveLength(1);
    expect(fake.find("GET", "/api/me")).toHaveLength(1);
  });
});

const detectedInputs = {
  branch: "main",
  skills: [],
  detected: [
    { name: "agent-rules", folder: ".agents/skills", on: false },
    { name: "lint", folder: ".claude/skills", on: false },
    { name: "broken", folder: ".github/skills", problem: "frontmatter does not parse", on: false },
  ],
  instructions: "",
};

function renderAt(path: string, routes: Parameters<typeof fakeApi>[0]) {
  fakeApi(routes);
  return renderApp(path);
}
