import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { fakeApi } from "../test/fakeApi";
import { admin, developer, librarySkills, repoDetail, repoPayments, repoWidgets, reviewBlocking } from "../test/fixtures";
import { renderApp } from "../test/render";

afterEach(() => vi.unstubAllGlobals());

const payments = repoDetail(repoPayments);
const widgets = repoDetail(repoWidgets);

const base = {
  "GET /api/me": { body: admin },
  "GET /api/repositories/main/acme/payments": { body: payments },
  "GET /api/repositories/main/acme/widgets": { body: widgets },
  "GET /api/skills": { body: librarySkills },
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

  it("hides Settings from a developer", async () => {
    renderAt("/repos/main/acme/payments?tab=settings", { ...base, "GET /api/me": { body: developer } });
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
    expect(screen.getByRole("spinbutton", { name: "Timeout (seconds)" })).toHaveValue(600);
    const memory = screen.getByRole("textbox", { name: "Memory limit" });
    expect(memory).toHaveValue("6Gi");
    expect(within(memory.closest(".set") as HTMLElement).getByText("overridden here")).toBeInTheDocument();
    // skills null: every library skill is on.
    expect(await screen.findByRole("switch", { name: "Skill code-review" })).toBeChecked();
    expect(screen.getByRole("switch", { name: "Skill security-review" })).toBeChecked();
    expect(screen.queryByText("managed by config")).not.toBeInTheDocument();
  });

  it("disables a config-managed repository's settings", async () => {
    const fake = fakeApi(base);
    renderApp("/repos/main/acme/widgets?tab=settings");
    expect(await screen.findAllByText("managed by config")).not.toHaveLength(0);
    await screen.findByRole("switch", { name: "Skill code-review" });
    for (const el of [
      screen.getByRole("switch", { name: "Approve clean reviews" }),
      screen.getByRole("switch", { name: "Review drafts" }),
      screen.getByRole("combobox", { name: "Status when a review fails" }),
      screen.getByRole("spinbutton", { name: "Timeout (seconds)" }),
      screen.getByRole("textbox", { name: "Memory limit" }),
      screen.getByRole("switch", { name: "Skill code-review" }),
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
        skills: { value: ["code-review"], overridden: true },
      },
    };
    const fake = fakeApi({ ...base, "PUT /api/repositories/main/acme/payments/settings": { body: saved } });
    renderApp("/repos/main/acme/payments?tab=settings");
    await userEvent.click(await screen.findByRole("switch", { name: "Approve clean reviews" }));
    const timeout = screen.getByRole("spinbutton", { name: "Timeout (seconds)" });
    await userEvent.clear(timeout);
    await userEvent.type(timeout, "900");
    await userEvent.click(await screen.findByRole("switch", { name: "Skill security-review" }));
    await userEvent.click(screen.getByRole("button", { name: "Save" }));

    const [put] = fake.find("PUT", "/api/repositories/main/acme/payments/settings");
    expect(put.headers.get("X-CSRF-Token")).toBe("csrf-123");
    expect(put.body).toEqual({ approve: false, jobTimeoutSeconds: 900, skills: ["code-review"] });
    expect(await screen.findByText("Saved.")).toBeInTheDocument();
    const timeoutRow = screen.getByRole("spinbutton", { name: "Timeout (seconds)" }).closest(".set") as HTMLElement;
    expect(within(timeoutRow).getByText("overridden here")).toBeInTheDocument();
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

  it("shows the resolved skills and instructions", async () => {
    fakeApi({
      ...base,
      "GET /api/repositories/main/acme/payments/inputs": {
        body: {
          branch: "main",
          skills: [{ name: "code-review", source: "repo", content: "Repo's own <b>review</b>." }],
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
      "GET /api/me": () => ({ body: mes++ === 0 ? admin : { ...admin, csrfToken: "csrf-new" } }),
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

function renderAt(path: string, routes: Parameters<typeof fakeApi>[0]) {
  fakeApi(routes);
  return renderApp(path);
}
