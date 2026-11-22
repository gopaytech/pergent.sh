import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { fakeApi } from "../test/fakeApi";
import { builtInRules, ruleSetGoAPI, ruleSets, ssoAdmin, viewer } from "../test/fixtures";
import { renderApp } from "../test/render";

afterEach(() => vi.unstubAllGlobals());

const rowOf = (name: string) => screen.getByRole("link", { name }).closest("[data-rule-set]") as HTMLElement;

describe("Rules", () => {
  it("lists the sets with their entries, the default and the config badge", async () => {
    fakeApi({ "GET /api/me": { body: viewer }, "GET /api/rule-sets": { body: ruleSets } });
    renderApp("/rules");
    expect(await screen.findByRole("heading", { name: "Rules" })).toBeInTheDocument();
    await screen.findByRole("link", { name: "go-api" });
    const stored = rowOf("go-api");
    expect(stored).toHaveTextContent("2 entries");
    expect(stored).toHaveTextContent("v2");
    expect(within(stored).getByText("default")).toHaveClass("pill");
    const config = rowOf("team");
    expect(config).toHaveTextContent("1 entry");
    expect(within(config).getByText("config file")).toHaveClass("pill");
    expect(config).toHaveTextContent("read-only");
  });

  it("an admin sees New rule set, Edit and Delete, not on a config set; a viewer sees none", async () => {
    fakeApi({ "GET /api/me": { body: ssoAdmin }, "GET /api/rule-sets": { body: ruleSets } });
    const { unmount } = renderApp("/rules");
    await screen.findByRole("link", { name: "go-api" });
    expect(screen.getByRole("link", { name: "New rule set" })).toHaveAttribute("href", "/rules/new");
    expect(within(rowOf("go-api")).getByRole("link", { name: "Edit" })).toHaveAttribute("href", "/rules/view/go-api");
    expect(within(rowOf("go-api")).getByRole("button", { name: "Delete" })).toBeInTheDocument();
    expect(within(rowOf("team")).queryByRole("button", { name: "Delete" })).not.toBeInTheDocument();
    unmount();

    fakeApi({ "GET /api/me": { body: viewer }, "GET /api/rule-sets": { body: ruleSets } });
    renderApp("/rules");
    await screen.findByRole("link", { name: "go-api" });
    expect(screen.queryByRole("link", { name: "New rule set" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Delete" })).not.toBeInTheDocument();
  });

  it("a delete of a set in use shows who uses it", async () => {
    vi.spyOn(window, "confirm").mockReturnValue(true);
    fakeApi({
      "GET /api/me": { body: ssoAdmin },
      "GET /api/rule-sets": { body: ruleSets },
      "DELETE /api/admin/rule-sets/go-api": {
        status: 409,
        body: { error: "the rule set is in use", repositories: [{ instance: "main", project: "acme/widgets" }], inDefaults: true },
      },
    });
    renderApp("/rules");
    await screen.findByRole("link", { name: "go-api" });
    await userEvent.click(within(rowOf("go-api")).getByRole("button", { name: "Delete" }));
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent("main: acme/widgets");
    expect(alert).toHaveTextContent("the default rule sets");
  });

  it("the Built-in tab lists pergent's rules, and says when there are none", async () => {
    fakeApi({
      "GET /api/me": { body: viewer },
      "GET /api/rule-sets": { body: ruleSets },
      "GET /api/built-in-rules": { body: [] },
    });
    const { unmount } = renderApp("/rules");
    await userEvent.click(await screen.findByRole("tab", { name: "Built-in" }));
    expect(await screen.findByText("No built-in rules yet.")).toBeInTheDocument();
    unmount();

    fakeApi({
      "GET /api/me": { body: viewer },
      "GET /api/rule-sets": { body: ruleSets },
      "GET /api/built-in-rules": { body: builtInRules },
    });
    renderApp("/rules");
    await userEvent.click(await screen.findByRole("tab", { name: "Built-in" }));
    // One row per rule, with all its globs and its text once.
    const rows = (await screen.findByRole("region", { name: "Built-in rules" })).querySelectorAll("[data-built-in]");
    expect([...rows].map((r) => r.getAttribute("data-built-in"))).toEqual(["github_workflows", "ts_js"]);
    const ts = rows[1] as HTMLElement;
    expect(within(ts).getByText("ts_js")).toBeInTheDocument();
    expect(within(ts).getByText("**/*.ts")).toBeInTheDocument();
    expect(within(ts).getByText("**/*.js")).toBeInTheDocument();
    expect(within(ts).getAllByText("Check for floating promises.")).toHaveLength(1);
    expect(screen.getAllByText("Check for floating promises.")).toHaveLength(1);
    expect(within(rows[0] as HTMLElement).getByText(".github/workflows/**/*.yml")).toBeInTheDocument();
    // They are chosen per repository, and the tab says where.
    expect(screen.getByText(/A repository gets none until you choose them: tick the ones it needs under Built-in rules on the repository's Settings/)).toBeInTheDocument();
    // The texts are adapted from open-code-review, and keep its license.
    expect(screen.getByRole("link", { name: "alibaba/open-code-review" })).toHaveAttribute("href", "https://github.com/alibaba/open-code-review");
    expect(screen.getByText(/Apache-2\.0/)).toBeInTheDocument();
  });
});

describe("RuleSetEditor", () => {
  it("saves the draft against the version it opened, and shows the notes", async () => {
    const fake = fakeApi({
      "GET /api/me": { body: ssoAdmin },
      "GET /api/rule-sets/go-api": { body: ruleSetGoAPI },
      "PUT /api/admin/rule-sets/go-api": { body: { version: 3, notes: ["entry 1: This entry hides the built-in rule go for matching files."] } },
    });
    renderApp("/rules/view/go-api");
    const text = await screen.findByLabelText("Rules");
    expect(text).toHaveValue(ruleSetGoAPI.content);
    await userEvent.type(text, "#");
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    const [put] = fake.find("PUT", "/api/admin/rule-sets/go-api");
    expect(put.body).toEqual({ content: ruleSetGoAPI.content + "#", version: 2 });
    expect(await screen.findByText("Saved as v3.")).toBeInTheDocument();
    expect(screen.getByText("entry 1: This entry hides the built-in rule go for matching files.")).toBeInTheDocument();
  });

  it("shows the loader's message when a save is refused", async () => {
    fakeApi({
      "GET /api/me": { body: ssoAdmin },
      "GET /api/rule-sets/go-api": { body: ruleSetGoAPI },
      "PUT /api/admin/rule-sets/go-api": { status: 400, body: { error: 'rule set "go-api": entry 1: path: pattern "api/" can never match a file' } },
    });
    renderApp("/rules/view/go-api");
    await userEvent.type(await screen.findByLabelText("Rules"), "x");
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("entry 1: path: pattern");
  });

  it("a config set and a viewer get the text read-only", async () => {
    fakeApi({ "GET /api/me": { body: ssoAdmin }, "GET /api/rule-sets/team": { body: { ...ruleSetGoAPI, name: "team", source: "config", version: 0 } } });
    const { unmount } = renderApp("/rules/view/team");
    expect(await screen.findByText("This rule set is defined in the config file. Change it there.")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Save" })).not.toBeInTheDocument();
    unmount();

    fakeApi({ "GET /api/me": { body: viewer }, "GET /api/rule-sets/go-api": { body: ruleSetGoAPI } });
    renderApp("/rules/view/go-api");
    await screen.findByRole("heading", { name: /go-api/ });
    expect(screen.queryByRole("button", { name: "Save" })).not.toBeInTheDocument();
  });

  it("creates a new set and opens it", async () => {
    const fake = fakeApi({
      "GET /api/me": { body: ssoAdmin },
      "POST /api/admin/rule-sets": { status: 201, body: { version: 1, notes: [] } },
      "GET /api/rule-sets/mine": { body: { ...ruleSetGoAPI, name: "mine", version: 1 } },
    });
    renderApp("/rules/new");
    await userEvent.type(await screen.findByLabelText("Name"), "mine");
    await userEvent.click(screen.getByRole("button", { name: "Create rule set" }));
    const [post] = fake.find("POST", "/api/admin/rule-sets");
    expect((post.body as { name: string }).name).toBe("mine");
    expect(await screen.findByRole("heading", { name: /mine/ })).toBeInTheDocument();
  });
});
