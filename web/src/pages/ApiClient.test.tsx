import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { fakeApi } from "../test/fakeApi";
import { apiClientFastgateway, apiKeyFresh, librarySkills, models, operator, viewer } from "../test/fixtures";
import { renderApp } from "../test/render";

afterEach(() => vi.unstubAllGlobals());

const url = "/api/admin/api-clients/fastgateway";
const base = {
  "GET /api/me": { body: operator },
  "GET /api/skills": { body: librarySkills },
  "GET /api/admin/models": { body: models },
  [`GET ${url}`]: { body: apiClientFastgateway },
};

describe("API client: settings", () => {
  it("opens on the settings, filled from the client", async () => {
    fakeApi(base);
    renderApp("/api-clients/fastgateway");
    expect(await screen.findByRole("heading", { name: "fastgateway" })).toBeInTheDocument();
    expect(screen.getByRole("tab", { name: "Settings" })).toHaveAttribute("aria-selected", "true");
    expect(await screen.findByRole("switch", { name: "Skill code-review" })).toBeChecked();
    expect(screen.getByRole("switch", { name: "Skill security-review" })).not.toBeChecked();
    expect(screen.getByLabelText("Model")).toHaveValue("");
    expect(screen.getByLabelText("Callback URL")).toHaveValue("https://fastgateway.example.com/hooks/pergent");
    expect(screen.getByLabelText("Max request bytes")).toHaveValue(524288);
    expect(screen.getByLabelText("Monthly token budget")).toHaveValue(10_000_000);
  });

  it("saves the settings with exactly {skills, modelId, callbackUrl, limits}", async () => {
    const fake = fakeApi({ ...base, [`PUT ${url}`]: { body: apiClientFastgateway } });
    renderApp("/api-clients/fastgateway");
    await userEvent.click(await screen.findByRole("switch", { name: "Skill security-review" }));
    await userEvent.selectOptions(screen.getByLabelText("Model"), "m-opus");
    const urlField = screen.getByLabelText("Callback URL");
    await userEvent.clear(urlField);
    await userEvent.type(urlField, "https://example.com/cb");
    const files = screen.getByLabelText("Max files");
    await userEvent.clear(files);
    await userEvent.type(files, "300");
    await userEvent.click(screen.getByRole("button", { name: "Save settings" }));

    const [put] = fake.find("PUT", url);
    expect(put.body).toEqual({
      skills: ["code-review", "security-review"],
      modelId: "m-opus",
      callbackUrl: "https://example.com/cb",
      limits: { maxRequestBytes: 524288, maxFiles: 300, maxInFlight: 5, requestsPerHour: 600, monthlyTokenBudget: 10_000_000 },
    });
    expect(put.headers.get("X-CSRF-Token")).toBe("csrf-123");
    expect(await screen.findByText("Settings saved.")).toBeInTheDocument();
  });

  it("keeps a skill the library no longer has, and says so", async () => {
    const fake = fakeApi({
      ...base,
      [`GET ${url}`]: { body: { ...apiClientFastgateway, skills: ["code-review", "retired"] } },
      [`PUT ${url}`]: { body: apiClientFastgateway },
    });
    renderApp("/api-clients/fastgateway");
    const gone = await screen.findByRole("switch", { name: "Skill retired" });
    expect(gone).toBeChecked();
    expect(gone.closest(".skill")).toHaveTextContent("not in the library");
    await userEvent.click(screen.getByRole("button", { name: "Save settings" }));
    await screen.findByText("Settings saved.");
    expect((fake.find("PUT", url)[0].body as { skills: string[] }).skills).toEqual(["code-review", "retired"]);
  });

  it("shows the server's text when the settings are refused", async () => {
    fakeApi({ ...base, [`PUT ${url}`]: { status: 400, body: { error: "max files must be from 1 to 1000" } } });
    renderApp("/api-clients/fastgateway");
    await userEvent.click(await screen.findByRole("button", { name: "Save settings" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("max files must be from 1 to 1000");
  });

  it("gives a user the not-found page and calls no admin route", async () => {
    const fake = fakeApi({ "GET /api/me": { body: viewer } });
    renderApp("/api-clients/fastgateway");
    expect(await screen.findByRole("heading", { name: "Not found" })).toBeInTheDocument();
    expect(fake.calls.filter((c) => c.url.startsWith("/api/admin/"))).toEqual([]);
  });

  it("gives the not-found page for a client that is not there", async () => {
    fakeApi({ ...base, [`GET ${url}`]: { status: 404, body: { error: "not found" } } });
    renderApp("/api-clients/fastgateway");
    expect(await screen.findByRole("heading", { name: "Not found" })).toBeInTheDocument();
  });

  it("rotates the callback secret, shows the new one once and says the old one still works", async () => {
    vi.stubGlobal("confirm", vi.fn(() => true));
    const fake = fakeApi({
      ...base,
      [`POST ${url}/secret`]: { body: { callbackSecret: "whsec_rotated" } },
      // Once rotated, the client says until when the old one signs.
      [`GET ${url}`]: (() => {
        let n = 0;
        return () => ({
          body: n++ === 0 ? apiClientFastgateway : { ...apiClientFastgateway, previousSecretUntil: "2026-11-12T10:00:00Z" },
        });
      })(),
    });
    renderApp("/api-clients/fastgateway");
    await userEvent.click(await screen.findByRole("button", { name: "Rotate secret" }));
    expect(fake.find("POST", `${url}/secret`)).toHaveLength(1);
    const box = await screen.findByRole("region", { name: "New callback secret" });
    expect(box).toHaveTextContent("whsec_rotated");
    expect(box).toHaveTextContent("Copy this secret now: it is not shown again.");
    expect(await screen.findByText("The old secret also works until 2026-11-12 10:00 UTC.")).toBeInTheDocument();
    await userEvent.click(within(box).getByRole("button", { name: "Done" }));
    expect(document.body.textContent).not.toContain("whsec_rotated");
  });

  it("rotates nothing when the confirmation is declined", async () => {
    vi.stubGlobal("confirm", vi.fn(() => false));
    const fake = fakeApi(base);
    renderApp("/api-clients/fastgateway");
    await userEvent.click(await screen.findByRole("button", { name: "Rotate secret" }));
    expect(fake.calls.filter((c) => c.method === "POST")).toEqual([]);
  });

  it("removes the client after a confirmation that says what goes, then returns to the list", async () => {
    const confirm = vi.fn(() => true);
    vi.stubGlobal("confirm", confirm);
    const fake = fakeApi({
      ...base,
      [`DELETE ${url}`]: { status: 204 },
      "GET /api/admin/api-clients": { body: { items: [] } },
    });
    renderApp("/api-clients/fastgateway");
    await userEvent.click(await screen.findByRole("button", { name: "Remove client" }));
    expect(confirm).toHaveBeenCalledWith(
      expect.stringContaining("Its keys stop at once, its running reviews end with no callback, and its reviews and dismissals are deleted."),
    );
    expect(fake.find("DELETE", url)).toHaveLength(1);
    expect(await screen.findByTestId("location")).toHaveTextContent(/^\/api-clients$/);
    expect(await screen.findByText(/No API clients yet/)).toBeInTheDocument();
  });

  it("removes nothing when the confirmation is declined", async () => {
    vi.stubGlobal("confirm", vi.fn(() => false));
    const fake = fakeApi(base);
    renderApp("/api-clients/fastgateway");
    await userEvent.click(await screen.findByRole("button", { name: "Remove client" }));
    expect(fake.calls.filter((c) => c.method === "DELETE")).toEqual([]);
  });
});

describe("API client: keys", () => {
  const openKeys = async () => {
    renderApp("/api-clients/fastgateway");
    await userEvent.click(await screen.findByRole("tab", { name: "Keys" }));
  };

  it("lists each key's name, prefix, created, expires and last used, never when null", async () => {
    fakeApi(base);
    await openKeys();
    const used = (await screen.findByText("ci")).closest("tr") as HTMLElement;
    expect(used).toHaveTextContent("pgk_ab12cd34");
    expect(used).toHaveTextContent("2026-11-01 08:00 UTC");
    expect(used).toHaveTextContent("2027-01-01 00:00 UTC");
    expect(used).toHaveTextContent("2026-11-10 09:30 UTC");
    const fresh = screen.getByText("staging").closest("tr") as HTMLElement;
    expect(fresh).toHaveTextContent("pgk_ef56gh78");
    expect(within(fresh).getAllByText("never")).toHaveLength(2);
  });

  it("creates a key and shows it once", async () => {
    let reads = 0;
    const fake = fakeApi({
      ...base,
      [`GET ${url}`]: () => ({
        body: reads++ === 0 ? { ...apiClientFastgateway, keys: [] } : apiClientFastgateway,
      }),
      [`POST ${url}/keys`]: { status: 201, body: { key: apiKeyFresh, secret: "pgk_ef56gh78_the-whole-key" } },
    });
    await openKeys();
    await userEvent.type(await screen.findByLabelText("Key name"), "staging");
    await userEvent.click(screen.getByRole("button", { name: "Create key" }));

    const [post] = fake.find("POST", `${url}/keys`);
    expect(post.body).toEqual({ name: "staging", expiresAt: null });
    expect(post.headers.get("X-CSRF-Token")).toBe("csrf-123");
    const box = await screen.findByRole("region", { name: "New API key" });
    expect(box).toHaveTextContent("pgk_ef56gh78_the-whole-key");
    expect(box).toHaveTextContent("Copy this secret now: it is not shown again.");
    expect(within(box).getByRole("button", { name: "Copy API key" })).toBeInTheDocument();
    // The list behind the box has the key by its name and prefix, not by the key.
    expect(await screen.findByText("pgk_ef56gh78")).toBeInTheDocument();

    await userEvent.click(within(box).getByRole("button", { name: "Done" }));
    expect(document.body.textContent).not.toContain("pgk_ef56gh78_the-whole-key");
  });

  it("sends an expiry as the end of that day in UTC", async () => {
    const fake = fakeApi({
      ...base,
      [`POST ${url}/keys`]: { status: 201, body: { key: apiKeyFresh, secret: "k" } },
    });
    await openKeys();
    await userEvent.type(await screen.findByLabelText("Key name"), "short");
    await userEvent.type(screen.getByLabelText("Expires (optional)"), "2027-03-04");
    await userEvent.click(screen.getByRole("button", { name: "Create key" }));
    await screen.findByRole("region", { name: "New API key" });
    expect(fake.find("POST", `${url}/keys`)[0].body).toEqual({ name: "short", expiresAt: "2027-03-04T23:59:59.000Z" });
  });

  it("revokes a key after a confirmation", async () => {
    const confirm = vi.fn(() => true);
    vi.stubGlobal("confirm", confirm);
    const fake = fakeApi({ ...base, [`DELETE ${url}/keys/7`]: { status: 204 } });
    await openKeys();
    const row = (await screen.findByText("ci")).closest("tr") as HTMLElement;
    await userEvent.click(within(row).getByRole("button", { name: "Revoke" }));
    expect(confirm).toHaveBeenCalledWith(expect.stringContaining("ci"));
    expect(fake.find("DELETE", `${url}/keys/7`)).toHaveLength(1);
    expect(fake.find("DELETE", `${url}/keys/7`)[0].headers.get("X-CSRF-Token")).toBe("csrf-123");
  });

  it("revokes nothing when the confirmation is declined", async () => {
    vi.stubGlobal("confirm", vi.fn(() => false));
    const fake = fakeApi(base);
    await openKeys();
    const row = (await screen.findByText("ci")).closest("tr") as HTMLElement;
    await userEvent.click(within(row).getByRole("button", { name: "Revoke" }));
    expect(fake.calls.filter((c) => c.method === "DELETE")).toEqual([]);
  });
});
