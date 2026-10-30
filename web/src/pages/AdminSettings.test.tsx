import { screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { fakeApi } from "../test/fakeApi";
import { admin, adminSettings, developer } from "../test/fixtures";
import { renderApp } from "../test/render";

afterEach(() => vi.unstubAllGlobals());

describe("AdminSettings", () => {
  it("shows the defaults, the model and the admins", async () => {
    fakeApi({ "GET /api/me": { body: admin }, "GET /api/admin/settings": { body: adminSettings } });
    renderApp("/settings");
    expect(await screen.findByRole("heading", { name: "Settings" })).toBeInTheDocument();
    const defaults = await screen.findByRole("region", { name: "Repository defaults" });
    expect(defaults).toHaveTextContent("Approve clean reviewsOn");
    expect(defaults).toHaveTextContent("Status when a review failsneutral");
    expect(defaults).toHaveTextContent("Timeout10 min");
    expect(defaults).toHaveTextContent("Memory limitnone");
    expect(screen.getByRole("region", { name: "Model" })).toHaveTextContent("anthropic");
    expect(screen.getByRole("region", { name: "Model" })).toHaveTextContent("claude-sonnet");
    expect(screen.getByRole("region", { name: "Admins" })).toHaveTextContent("group acme/platform");
  });

  it("isn't shown to a developer", async () => {
    const fake = fakeApi({ "GET /api/me": { body: developer } });
    renderApp("/settings");
    expect(await screen.findByRole("heading", { name: "Not found" })).toBeInTheDocument();
    expect(fake.find("GET", "/api/admin/settings")).toHaveLength(0);
  });
});
