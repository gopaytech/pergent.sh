import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { fakeApi } from "../test/fakeApi";
import { apiClientFastgateway, apiClientRows, librarySkills, models, operator, viewer } from "../test/fixtures";
import { renderApp } from "../test/render";

afterEach(() => vi.unstubAllGlobals());

const base = {
  "GET /api/me": { body: operator },
  "GET /api/skills": { body: librarySkills },
  "GET /api/admin/models": { body: models },
};

describe("API clients", () => {
  it("lists each client with its key count, linked to its page", async () => {
    fakeApi({ ...base, "GET /api/admin/api-clients": { body: { items: apiClientRows } } });
    renderApp("/api-clients");
    expect(await screen.findByRole("heading", { name: "API clients" })).toBeInTheDocument();
    const link = await screen.findByRole("link", { name: "fastgateway" });
    expect(link).toHaveAttribute("href", "/api-clients/fastgateway");
    expect(link.closest("tr")).toHaveTextContent("2");
    expect(screen.getByRole("link", { name: "docs-bot" }).closest("tr")).toHaveTextContent("0");
  });

  it("says so when there are no clients", async () => {
    fakeApi({ ...base, "GET /api/admin/api-clients": { body: { items: [] } } });
    renderApp("/api-clients");
    expect(await screen.findByText(/No API clients yet/)).toBeInTheDocument();
  });

  it("creates a client with no limits in the body and shows the callback secret once", async () => {
    let listed = 0;
    const fake = fakeApi({
      ...base,
      "GET /api/admin/api-clients": () => ({ body: { items: listed++ === 0 ? [] : apiClientRows } }),
      "POST /api/admin/api-clients": {
        status: 201,
        body: { client: { ...apiClientFastgateway, keys: [] }, callbackSecret: "whsec_generated-once" },
      },
    });
    renderApp("/api-clients");
    await userEvent.click(await screen.findByRole("button", { name: "New client" }));
    await userEvent.type(screen.getByLabelText("Name"), "fastgateway");
    await userEvent.click(await screen.findByRole("switch", { name: "Skill code-review" }));
    await userEvent.type(screen.getByLabelText("Callback URL"), "https://fastgateway.example.com/hooks/pergent");
    await userEvent.click(screen.getByRole("button", { name: "Create client" }));

    const [post] = fake.find("POST", "/api/admin/api-clients");
    expect(post.body).toEqual({
      name: "fastgateway",
      skills: ["code-review"],
      modelId: "",
      callbackUrl: "https://fastgateway.example.com/hooks/pergent",
    });
    expect(post.headers.get("X-CSRF-Token")).toBe("csrf-123");

    const box = await screen.findByRole("region", { name: "New API client" });
    expect(box).toHaveTextContent("whsec_generated-once");
    expect(box).toHaveTextContent("Copy this secret now: it is not shown again.");
    expect(within(box).getByRole("button", { name: "Copy callback secret" })).toBeInTheDocument();
    // A labelled region, not a live region: a screen reader doesn't read the secret out unasked.
    expect(box.closest("[aria-live]")).toBeNull();
    expect(within(box).getByRole("link", { name: "Open fastgateway" })).toHaveAttribute("href", "/api-clients/fastgateway");

    await userEvent.click(within(box).getByRole("button", { name: "Done" }));
    expect(screen.queryByText("whsec_generated-once")).not.toBeInTheDocument();
    expect(document.body.textContent).not.toContain("whsec_generated-once");
  });

  it("sends the model a client was given", async () => {
    const fake = fakeApi({
      ...base,
      "GET /api/admin/api-clients": { body: { items: [] } },
      "POST /api/admin/api-clients": { status: 201, body: { client: apiClientFastgateway, callbackSecret: "s" } },
    });
    renderApp("/api-clients");
    await userEvent.click(await screen.findByRole("button", { name: "New client" }));
    await userEvent.type(screen.getByLabelText("Name"), "fastgateway");
    await userEvent.selectOptions(await screen.findByLabelText("Model"), "m-opus");
    await userEvent.click(screen.getByRole("button", { name: "Create client" }));
    await screen.findByRole("region", { name: "New API client" });
    expect(fake.find("POST", "/api/admin/api-clients")[0].body).toEqual({
      name: "fastgateway",
      skills: [],
      modelId: "m-opus",
      callbackUrl: "",
    });
  });

  it("shows the server's text when the name is taken", async () => {
    fakeApi({
      ...base,
      "GET /api/admin/api-clients": { body: { items: apiClientRows } },
      "POST /api/admin/api-clients": { status: 409, body: { error: "an API client with this name exists" } },
    });
    renderApp("/api-clients");
    await userEvent.click(await screen.findByRole("button", { name: "New client" }));
    await userEvent.type(screen.getByLabelText("Name"), "fastgateway");
    await userEvent.click(screen.getByRole("button", { name: "Create client" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("an API client with this name exists");
  });

  it("gives a user the not-found page and calls no admin route", async () => {
    const fake = fakeApi({ "GET /api/me": { body: viewer } });
    renderApp("/api-clients");
    expect(await screen.findByRole("heading", { name: "Not found" })).toBeInTheDocument();
    expect(fake.calls.filter((c) => c.url.startsWith("/api/admin/"))).toEqual([]);
  });
});
