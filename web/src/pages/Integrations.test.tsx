import { screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { fakeApi } from "../test/fakeApi";
import { admin, integrations } from "../test/fixtures";
import { renderApp } from "../test/render";

afterEach(() => vi.unstubAllGlobals());

describe("Integrations", () => {
  it("shows the instance, the approver and each repository's installation", async () => {
    fakeApi({ "GET /api/me": { body: admin }, "GET /api/integrations": { body: integrations } });
    renderApp("/integrations");
    expect(await screen.findByRole("heading", { name: "Integrations" })).toBeInTheDocument();
    const inst = await screen.findByRole("region", { name: "GitLab · main" });
    expect(within(inst).getByText("Connected")).toHaveClass("pill", "ok");
    expect(inst).toHaveTextContent("Approver: bot user token");
    expect(inst).toHaveTextContent("Token expires 2026-11-09");
    const row = screen.getByText("acme/payments").closest("tr") as HTMLElement;
    expect(within(row).getByText("Installed")).toHaveClass("pill", "ok");
    expect(within(row).getByText("Can't approve")).toHaveClass("pill", "bad");
  });
});
