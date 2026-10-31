import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { fakeApi } from "../test/fakeApi";
import { defaults, llm, operator, platformAdmin, viewer } from "../test/fixtures";
import { renderApp } from "../test/render";

afterEach(() => vi.unstubAllGlobals());

const routes = (me = platformAdmin) => ({
  "GET /api/me": { body: me },
  "GET /api/admin/defaults": { body: defaults },
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
    expect(within(form).getByLabelText("Memory limit")).toHaveValue("");
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
      jobMemory: "6Gi",
    });
    expect(await screen.findByText("Defaults saved.")).toBeInTheDocument();
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
    expect(key).toHaveAttribute("autocomplete", "off");
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
    await userEvent.type(within(form).getByLabelText("Provider"), "anthropic");
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
