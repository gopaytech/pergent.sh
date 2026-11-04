import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { fakeApi } from "../test/fakeApi";
import { defaults, librarySkills, llm, operator, platformAdmin, viewer } from "../test/fixtures";
import { renderApp } from "../test/render";

afterEach(() => vi.unstubAllGlobals());

const routes = (me = platformAdmin) => ({
  "GET /api/me": { body: me },
  "GET /api/admin/defaults": { body: defaults },
  "GET /api/skills": { body: librarySkills },
  "GET /api/admin/llm": { body: { ...llm, editable: false } },
});

describe("AdminSettings defaults", () => {
  it("shows the defaults in a form an admin can change", async () => {
    fakeApi(routes());
    renderApp("/settings");
    expect(await screen.findByRole("heading", { name: "Settings" })).toBeInTheDocument();
    const form = await screen.findByRole("form", { name: "Repository defaults" });
    expect(within(form).getByRole("switch", { name: "Approve clean reviews" })).toBeChecked();
    expect(within(form).getByRole("switch", { name: "Review drafts" })).toBeChecked();
    expect(within(form).getByLabelText("Status when a review fails")).toHaveValue("neutral");
    expect(within(form).getByLabelText("Timeout (seconds)")).toHaveValue(600);
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
    renderApp("/settings");
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
      reviewDrafts: true,
      failureStatus: "neutral",
      jobTimeoutSeconds: 900,
      // Untouched, but the PUT replaces every default.
      maxTurns: 40,
      jobMemory: "6Gi",
      // Untouched, but the PUT replaces every default: saving without
      // them would reset the caps.
      rerunLimitPerMR: 10,
      rerunLimitPerRepo: 50,
      // Untouched too: the default skills are sent as they are.
      skills: ["code-review"],
    });
    const saved = await screen.findByText("Defaults saved.");
    expect(saved).not.toHaveClass("problem");
  });

  it("saves changed re-run caps", async () => {
    const fake = fakeApi({
      ...routes(),
      "PUT /api/admin/defaults": { body: { ...defaults, rerunLimitPerMR: 3, rerunLimitPerRepo: 120 } },
    });
    renderApp("/settings");
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

  it("sends an emptied re-run cap as null, the built-in default", async () => {
    const fake = fakeApi({ ...routes(), "PUT /api/admin/defaults": { body: defaults } });
    renderApp("/settings");
    const form = await screen.findByRole("form", { name: "Repository defaults" });
    await userEvent.clear(within(form).getByLabelText("Re-run cap per repository (per day)"));
    await userEvent.click(within(form).getByRole("button", { name: "Save defaults" }));
    expect(fake.find("PUT", "/api/admin/defaults")[0].body).toMatchObject({ rerunLimitPerMR: 10, rerunLimitPerRepo: null });
  });

  it.each(["0", "1001", "1.5"])("refuses the re-run cap %s without asking the server", async (value) => {
    const fake = fakeApi(routes());
    renderApp("/settings");
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
    renderApp("/settings");
    const form = await screen.findByRole("form", { name: "Repository defaults" });
    await userEvent.click(within(form).getByRole("button", { name: "Save defaults" }));
    expect(fake.find("PUT", "/api/admin/defaults")[0].body).toMatchObject({ jobMemory: null });
  });

  it("refuses a timeout that isn't a whole number without asking the server", async () => {
    const fake = fakeApi(routes());
    renderApp("/settings");
    const form = await screen.findByRole("form", { name: "Repository defaults" });
    await userEvent.clear(within(form).getByLabelText("Timeout (seconds)"));
    await userEvent.click(within(form).getByRole("button", { name: "Save defaults" }));
    expect(within(form).getByText(/whole number of seconds/)).toBeInTheDocument();
    expect(fake.calls.filter((c) => c.method === "PUT")).toHaveLength(0);
  });

  it("sends a changed Max steps with the other defaults", async () => {
    const fake = fakeApi({ ...routes(), "PUT /api/admin/defaults": { body: { ...defaults, maxTurns: 90 } } });
    renderApp("/settings");
    const form = await screen.findByRole("form", { name: "Repository defaults" });
    await userEvent.clear(within(form).getByLabelText("Max steps"));
    await userEvent.type(within(form).getByLabelText("Max steps"), "90");
    await userEvent.click(within(form).getByRole("button", { name: "Save defaults" }));
    expect(fake.find("PUT", "/api/admin/defaults")[0].body).toMatchObject({ maxTurns: 90, jobTimeoutSeconds: 600 });
  });

  it.each(["4", "201", ""])("refuses Max steps %j without asking the server", async (text) => {
    const fake = fakeApi(routes());
    renderApp("/settings");
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
    renderApp("/settings");
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
    renderApp("/settings");
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
    renderApp("/settings");
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
    renderApp("/settings");
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
    renderApp("/settings");
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
    renderApp("/settings");
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
    renderApp("/settings");
    const form = await screen.findByRole("form", { name: "Repository defaults" });
    expect(await within(form).findByText("boom")).toBeInTheDocument();
    await userEvent.click(within(form).getByRole("button", { name: "Save defaults" }));
    // Left out, the API keeps the stored list.
    expect(fake.find("PUT", "/api/admin/defaults")[0].body).not.toHaveProperty("skills");
  });
});

describe("AdminSettings model", () => {
  it("is read-only for a platform admin, without a key", async () => {
    fakeApi(routes());
    renderApp("/settings");
    const model = await screen.findByRole("region", { name: "Model" });
    expect(model).toHaveTextContent("anthropic");
    expect(model).toHaveTextContent("claude-sonnet");
    expect(model).toHaveTextContent("set · changed 3 days ago");
    expect(within(model).queryByRole("textbox")).not.toBeInTheDocument();
    expect(within(model).queryByRole("button")).not.toBeInTheDocument();
    expect(model).not.toHaveTextContent("from the config file");
  });

  it("says so when the model comes from the config file", async () => {
    fakeApi({
      ...routes(operator),
      "GET /api/admin/llm": { body: { ...llm, source: "config", editable: false, key: { set: true, updatedAt: null } } },
    });
    renderApp("/settings");
    const model = await screen.findByRole("region", { name: "Model" });
    expect(model).toHaveTextContent("from the config file");
    expect(within(model).queryByRole("textbox")).not.toBeInTheDocument();
    expect(model).toHaveTextContent("Key");
    expect(model).toHaveTextContent("set");
  });

  it("is editable for an operator, with a write-only key", async () => {
    let read = 0;
    const fake = fakeApi({
      ...routes(operator),
      "GET /api/admin/llm": () => ({ body: read++ === 0 ? llm : { ...llm, model: "claude-opus" } }),
      "PUT /api/admin/llm": { body: { ...llm, model: "claude-opus" } },
    });
    renderApp("/settings");
    const form = await screen.findByRole("form", { name: "Model" });
    expect(within(form).getByLabelText("Provider")).toHaveValue("anthropic");
    expect(within(form).getByLabelText("Model")).toHaveValue("claude-sonnet");
    expect(within(form).getByLabelText("Base URL")).toHaveValue("");
    // A key that is set is not shown, nor its input, until Replace.
    expect(form).toHaveTextContent("set · changed 3 days ago");
    expect(within(form).queryByLabelText("New API key")).not.toBeInTheDocument();
    await userEvent.clear(within(form).getByLabelText("Model"));
    await userEvent.type(within(form).getByLabelText("Model"), "claude-opus");
    await userEvent.click(within(form).getByRole("button", { name: "Save model" }));
    const [put] = fake.find("PUT", "/api/admin/llm");
    expect(put.headers.get("X-CSRF-Token")).toBe("csrf-123");
    expect(put.body).toEqual({ provider: "anthropic", model: "claude-opus", baseURL: "" });
    expect(await screen.findByText("Model saved.")).toBeInTheDocument();
  });

  it("sends a replacement key from an empty password input, and forgets it", async () => {
    const fake = fakeApi({ ...routes(operator), "GET /api/admin/llm": { body: llm }, "PUT /api/admin/llm": { body: llm } });
    renderApp("/settings");
    const form = await screen.findByRole("form", { name: "Model" });
    await userEvent.click(within(form).getByRole("button", { name: "Replace" }));
    const key = within(form).getByLabelText("New API key");
    expect(key).toHaveAttribute("type", "password");
    // "off" doesn't stop a password manager filling the operator's own
    // sign-in here; "new-password" and the managers' opt-outs do.
    expect(key).toHaveAttribute("autocomplete", "new-password");
    expect(key).toHaveAttribute("data-1p-ignore");
    expect(key).toHaveAttribute("data-lpignore", "true");
    expect(key).toHaveValue("");
    await userEvent.type(key, "sk-new-key");
    await userEvent.click(within(form).getByRole("button", { name: "Save model" }));
    expect(fake.find("PUT", "/api/admin/llm")[0].body).toEqual({
      provider: "anthropic",
      model: "claude-sonnet",
      baseURL: "",
      apiKey: "sk-new-key",
    });
    await vi.waitFor(() => expect(screen.queryByLabelText("New API key")).not.toBeInTheDocument());
    expect(document.body.textContent).not.toContain("sk-new-key");
  });

  it("asks for the key straight away when none is stored", async () => {
    const fake = fakeApi({
      ...routes(operator),
      "GET /api/admin/llm": { body: { ...llm, provider: "", model: "", key: { set: false, updatedAt: null } } },
      "PUT /api/admin/llm": { body: llm },
    });
    renderApp("/settings");
    const form = await screen.findByRole("form", { name: "Model" });
    expect(form).toHaveTextContent("not set");
    await userEvent.selectOptions(within(form).getByLabelText("Provider"), "anthropic");
    await userEvent.type(within(form).getByLabelText("Model"), "claude-sonnet");
    await userEvent.type(within(form).getByLabelText("Base URL"), "https://llm.example.com");
    await userEvent.type(within(form).getByLabelText("New API key"), "sk-first");
    await userEvent.click(within(form).getByRole("button", { name: "Save model" }));
    expect(fake.find("PUT", "/api/admin/llm")[0].body).toEqual({
      provider: "anthropic",
      model: "claude-sonnet",
      baseURL: "https://llm.example.com",
      apiKey: "sk-first",
    });
  });

  it("picks the provider from a list", async () => {
    const fake = fakeApi({
      ...routes(operator),
      "GET /api/admin/llm": { body: { ...llm, provider: "", model: "", key: { set: false, updatedAt: null } } },
      "PUT /api/admin/llm": { body: llm },
    });
    renderApp("/settings");
    const form = await screen.findByRole("form", { name: "Model" });
    const provider = within(form).getByLabelText("Provider");
    const options = within(provider)
      .getAllByRole("option")
      .map((o) => (o as HTMLOptionElement).value);
    expect(options).toEqual(
      expect.arrayContaining(["anthropic", "openai", "deepseek", "google", "openrouter", "groq", "mistral", "xai", "other"]),
    );
    // Nothing is picked until the operator picks.
    expect(provider).toHaveValue("");
    expect(within(form).queryByLabelText("Provider ID")).not.toBeInTheDocument();
    await userEvent.selectOptions(provider, "deepseek");
    await userEvent.type(within(form).getByLabelText("Model"), "deepseek-chat");
    await userEvent.type(within(form).getByLabelText("New API key"), "sk-ds");
    await userEvent.click(within(form).getByRole("button", { name: "Save model" }));
    expect(fake.find("PUT", "/api/admin/llm")[0].body).toEqual({
      provider: "deepseek",
      model: "deepseek-chat",
      baseURL: "",
      apiKey: "sk-ds",
    });
  });

  it("takes any other provider ID, and shows a stored one that isn't listed", async () => {
    const fake = fakeApi({
      ...routes(operator),
      "GET /api/admin/llm": { body: { ...llm, provider: "my-gateway", baseURL: "https://llm.example.com/v1" } },
      "PUT /api/admin/llm": { body: llm },
    });
    renderApp("/settings");
    const form = await screen.findByRole("form", { name: "Model" });
    expect(within(form).getByLabelText("Provider")).toHaveValue("other");
    const id = within(form).getByLabelText("Provider ID");
    expect(id).toHaveValue("my-gateway");
    await userEvent.clear(id);
    await userEvent.type(id, "together");
    await userEvent.click(within(form).getByRole("button", { name: "Save model" }));
    expect(fake.find("PUT", "/api/admin/llm")[0].body).toEqual({
      provider: "together",
      model: "claude-sonnet",
      baseURL: "https://llm.example.com/v1",
    });
  });

  it("leaves the non-secret fields to no password manager either", async () => {
    fakeApi({ ...routes(operator), "GET /api/admin/llm": { body: llm } });
    renderApp("/settings");
    const form = await screen.findByRole("form", { name: "Model" });
    // A password manager fills a saved username into the text field before
    // a password field: the Base URL once showed "admin".
    for (const label of ["Model", "Base URL"]) {
      const input = within(form).getByLabelText(label);
      expect(input).toHaveAttribute("autocomplete", "off");
      expect(input).toHaveAttribute("data-1p-ignore");
      expect(input).toHaveAttribute("data-lpignore", "true");
    }
  });

  it("shows the server's text when the model is refused", async () => {
    fakeApi({
      ...routes(operator),
      "GET /api/admin/llm": { body: llm },
      "PUT /api/admin/llm": { status: 409, body: { error: "the LLM is set in the config file; change it there" } },
    });
    renderApp("/settings");
    const form = await screen.findByRole("form", { name: "Model" });
    await userEvent.click(within(form).getByRole("button", { name: "Save model" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("the LLM is set in the config file");
  });
});

describe("AdminSettings access", () => {
  it("isn't shown to a viewer", async () => {
    const fake = fakeApi({ "GET /api/me": { body: viewer } });
    renderApp("/settings");
    expect(await screen.findByRole("heading", { name: "Not found" })).toBeInTheDocument();
    expect(fake.find("GET", "/api/admin/defaults")).toHaveLength(0);
    expect(fake.find("GET", "/api/admin/llm")).toHaveLength(0);
  });
});
