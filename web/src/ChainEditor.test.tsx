import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import type { ChainEntryView, ChainView } from "./api";
import ChainEditor, { entryStateText } from "./ChainEditor";
import MarkBanner from "./MarkBanner";
import { chainGlobal, choice, entryConfig, entryOpus, entrySonnet, entryTeamQwen } from "./test/fixtures";

const rows = () => within(screen.getByRole("list", { name: "Model chain" })).queryAllByRole("listitem");
const rowOf = (name: string) => rows().find((r) => within(r).queryByText(name)) as HTMLElement;
const names = () => rows().map((r) => r.querySelector("b")?.textContent);

function setup(chain: ChainView = chainGlobal, extra: { allowEmpty?: boolean; busy?: boolean } = {}) {
  const onSave = vi.fn();
  const onClear = vi.fn();
  render(<ChainEditor value={chain} choices={chain.choices} readOnly={chain.readOnly} onSave={onSave} onClear={onClear} {...extra} />);
  return { onSave, onClear };
}

describe("ChainEditor", () => {
  it("lists the entries in order, with whose each is", () => {
    setup({ ...chainGlobal, entries: [entryConfig, entrySonnet, { ...entryTeamQwen, position: 3 }] });
    expect(names()).toEqual(["openai/gpt-5", "Claude Sonnet", "qwen3.5"]);
    expect(rowOf("Claude Sonnet")).toHaveTextContent("anthropic / claude-sonnet · global");
    expect(rowOf("qwen3.5")).toHaveTextContent("openrouter / qwen/qwen3.5 · team Payments");
    expect(rowOf("openai/gpt-5")).toHaveTextContent("from the config file");
  });

  it("moves an entry and saves the new order, without the config file's entries", async () => {
    const { onSave } = setup({ ...chainGlobal, entries: [entryConfig, entrySonnet, entryOpus] });
    await userEvent.click(screen.getByRole("button", { name: "Move Claude Opus up" }));
    expect(names()).toEqual(["openai/gpt-5", "Claude Opus", "Claude Sonnet"]);
    await userEvent.click(screen.getByRole("button", { name: "Save chain" }));
    expect(onSave).toHaveBeenCalledWith([
      { source: "global", id: "m-opus" },
      { source: "global", id: "m-sonnet" },
    ]);
  });

  it("keeps the config file's entries first: they never move, and nothing moves above them", () => {
    setup({ ...chainGlobal, entries: [entryConfig, entrySonnet, entryOpus] });
    expect(screen.queryByRole("button", { name: "Move openai/gpt-5 up" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Move openai/gpt-5 down" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Remove openai/gpt-5" })).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Move Claude Sonnet up" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Move Claude Opus up" })).toBeEnabled();
    expect(screen.getByRole("button", { name: "Move Claude Opus down" })).toBeDisabled();
  });

  it("keeps the config file's entries in front of the global chain only", async () => {
    // In a repository's chain the config file's entry is one like any other.
    const repo: ChainView = {
      ...chainGlobal,
      scope: "repo",
      source: "repository",
      entries: [{ ...entrySonnet, position: 1 }, { ...entryConfig, position: 2 }],
      choices: [choice(entrySonnet), choice(entryConfig)],
    };
    const { onSave } = setup(repo);
    expect(rowOf("openai/gpt-5")).not.toHaveTextContent("from the config file");
    await userEvent.click(screen.getByRole("button", { name: "Move openai/gpt-5 up" }));
    await userEvent.click(screen.getByRole("button", { name: "Save chain" }));
    expect(onSave).toHaveBeenCalledWith([
      { source: "global", id: "config" },
      { source: "global", id: "m-sonnet" },
    ]);
  });

  it("adds a choice at the end and removes an entry", async () => {
    const { onSave } = setup();
    const pick = screen.getByRole("combobox", { name: "Entry to add" });
    // An entry in the chain is not offered again.
    expect(within(pick).queryByRole("option", { name: /Claude Sonnet/ })).not.toBeInTheDocument();
    await userEvent.selectOptions(pick, "Claude Opus (global)");
    await userEvent.click(screen.getByRole("button", { name: "Add" }));
    expect(names()).toEqual(["openai/gpt-5", "Claude Sonnet", "Claude Opus"]);
    await userEvent.click(screen.getByRole("button", { name: "Remove Claude Sonnet" }));
    await userEvent.click(screen.getByRole("button", { name: "Save chain" }));
    expect(onSave).toHaveBeenCalledWith([{ source: "global", id: "m-opus" }]);
  });

  it("turns Save off past 5 entries, counting the config file's, and says why", async () => {
    const more = [1, 2, 3, 4].map((n) => ({ ...entryOpus, id: `m-${n}`, name: `Model ${n}` }));
    const chain: ChainView = { ...chainGlobal, entries: [entryConfig, ...more], choices: [...more.map(choice), choice(entrySonnet)] };
    setup(chain);
    // 5 entries: Save works.
    expect(screen.getByRole("button", { name: "Save chain" })).toBeEnabled();
    await userEvent.selectOptions(screen.getByRole("combobox", { name: "Entry to add" }), "Claude Sonnet (global)");
    await userEvent.click(screen.getByRole("button", { name: "Add" }));
    expect(rows()).toHaveLength(6);
    expect(screen.getByRole("button", { name: "Save chain" })).toBeDisabled();
    expect(screen.getByText("A chain holds at most 5 entries. Remove one to save.")).toBeInTheDocument();
  });

  it("says what reviews do with each entry now", () => {
    const until = "2026-11-21T14:00:00Z";
    const entries: ChainEntryView[] = [
      { ...entrySonnet, position: 1, state: "ready" },
      { ...entryOpus, position: 2, state: "exhausted", class: "quota", until },
      { ...entryOpus, position: 3, id: "m-3", name: "Billing", state: "exhausted", class: "billing", until },
      { ...entryOpus, position: 4, id: "m-4", name: "Auth", state: "key_rejected", class: "auth" },
      { ...entryOpus, position: 5, id: "m-5", name: "Gone", state: "removed" },
    ];
    setup({ ...chainGlobal, readOnly: true, entries, choices: [] });
    expect(rowOf("Claude Sonnet")).toHaveTextContent("ready");
    expect(rowOf("Claude Opus")).toHaveTextContent("exhausted until 2026-11-21 14:00 UTC (quota)");
    expect(rowOf("Billing")).toHaveTextContent("exhausted until 2026-11-21 14:00 UTC (billing)");
    expect(rowOf("Auth")).toHaveTextContent("key rejected (auth)");
    expect(rowOf("Gone")).toHaveTextContent("removed");
    expect(entryStateText({ ...entryTeamQwen, state: "not_allowed" })).toBe("not allowed for teams any more");
    expect(entryStateText({ ...entryTeamQwen, state: "not_owner" })).toBe("team no longer owns this repository");
    expect(entryStateText({ ...entryTeamQwen, state: "key_unusable" })).toBe("key can't be read");
    expect(entryStateText({ ...entryTeamQwen, state: "model_limit" })).toBe("model limit used up");
    expect(entryStateText({ ...entryTeamQwen, state: "team_limit" })).toBe("team limit used up");
  });

  it("offers Clear mark only where the session may clear it, and calls back with the entry", async () => {
    const marked = { ...entryOpus, state: "exhausted" as const, class: "quota" as const, until: "2026-11-21T14:00:00Z" };
    const { onClear } = setup({
      ...chainGlobal,
      entries: [entryConfig, { ...marked, canClear: true }, { ...marked, id: "m-x", name: "Other", position: 4, canClear: false }],
    });
    expect(within(rowOf("Other")).queryByRole("button", { name: /Clear mark/ })).not.toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Clear mark of Claude Opus" }));
    expect(onClear).toHaveBeenCalledWith(expect.objectContaining({ source: "global", id: "m-opus" }));
  });

  it("shows a read-only chain with no buttons that change it", () => {
    setup({ ...chainGlobal, readOnly: true, choices: [] });
    expect(names()).toEqual(["openai/gpt-5", "Claude Sonnet"]);
    expect(screen.queryByRole("button", { name: "Save chain" })).not.toBeInTheDocument();
    expect(screen.queryByRole("combobox", { name: "Entry to add" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /^Move|^Remove/ })).not.toBeInTheDocument();
  });

  it("flags an entry the session may not keep, and turns Save off until it is removed", async () => {
    // An admin saved this chain with a global entry teams may not use; a
    // maintainer's choices don't have it.
    const kept = { ...entryOpus, position: 1 };
    const chain: ChainView = {
      scope: "repo",
      source: "repository",
      team: "",
      readOnly: false,
      savedByAdmin: true,
      entries: [kept, { ...entryTeamQwen, position: 2 }],
      choices: [choice(entryTeamQwen)],
    };
    const { onSave } = setup(chain);
    expect(rowOf("Claude Opus")).toHaveTextContent("You can't keep this entry here. Remove it to save.");
    expect(rowOf("qwen3.5")).not.toHaveTextContent("You can't keep");
    expect(screen.getByRole("button", { name: "Save chain" })).toBeDisabled();
    await userEvent.click(screen.getByRole("button", { name: "Remove Claude Opus" }));
    await userEvent.click(screen.getByRole("button", { name: "Save chain" }));
    expect(onSave).toHaveBeenCalledWith([{ source: "team", id: "t-qwen" }]);
  });

  it("turns Save off for an empty chain unless it may be empty", async () => {
    const one: ChainView = { ...chainGlobal, entries: [entrySonnet], choices: [choice(entrySonnet)] };
    const first = render(<ChainEditor value={one} choices={one.choices} readOnly={false} onSave={vi.fn()} onClear={vi.fn()} />);
    await userEvent.click(screen.getByRole("button", { name: "Remove Claude Sonnet" }));
    expect(screen.getByRole("button", { name: "Save chain" })).toBeDisabled();
    expect(screen.getByText("No entries.")).toBeInTheDocument();
    first.unmount();

    const onSave = vi.fn();
    render(<ChainEditor value={one} choices={one.choices} readOnly={false} allowEmpty onSave={onSave} onClear={vi.fn()} />);
    await userEvent.click(screen.getByRole("button", { name: "Remove Claude Sonnet" }));
    await userEvent.click(screen.getByRole("button", { name: "Save chain" }));
    expect(onSave).toHaveBeenCalledWith([]);
  });

  it("turns Save off while a save is on its way", () => {
    setup(chainGlobal, { busy: true });
    expect(screen.getByRole("button", { name: "Save chain" })).toBeDisabled();
  });
});

describe("MarkBanner", () => {
  it("names each entry with a live mark, and why", () => {
    const until = "2026-11-21T14:00:00Z";
    render(
      <MarkBanner
        entries={[
          { name: "Claude Sonnet", mark: null },
          { name: "Claude Opus", mark: { class: "quota", until, setAt: until, live: true } },
          { name: "Billing", mark: { class: "billing", until, setAt: until, live: true } },
          { name: "Auth", mark: { class: "auth", until: null, setAt: until, live: true } },
          { name: "Old", mark: { class: "quota", until, setAt: until, live: false } },
        ]}
      />,
    );
    const banner = screen.getByRole("note", { name: "Marked entries" });
    expect(banner).toHaveTextContent("Reviews skip these entries until the mark ends or someone clears it.");
    expect(banner).toHaveTextContent("Claude Opus: out of quota until 2026-11-21 14:00 UTC");
    expect(banner).toHaveTextContent("Billing: out of credit until 2026-11-21 14:00 UTC");
    expect(banner).toHaveTextContent("Auth: the key was rejected, until someone changes the key");
    expect(banner).not.toHaveTextContent("Old");
    expect(banner).not.toHaveTextContent("Claude Sonnet");
  });

  it("shows nothing while no entry has a live mark", () => {
    const { container } = render(<MarkBanner entries={[{ name: "Claude Sonnet", mark: null }]} />);
    expect(container).toBeEmptyDOMElement();
  });
});
