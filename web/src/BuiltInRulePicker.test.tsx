import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState } from "react";
import { describe, expect, it } from "vitest";

import type { BuiltInRule } from "./api";
import BuiltInRulePicker from "./BuiltInRulePicker";

const rules: BuiltInRule[] = [
  { name: "yaml", paths: ["**/*.yaml", "**/*.yml"], rule: "Check the keys." },
  { name: "go", paths: ["**/*.go"], rule: "Check the errors." },
  { name: "ts_js", paths: ["**/*.ts", "**/*.js"], rule: "Check the promises." },
];

// Host keeps the state the page keeps, so a change shows in the picker.
function Host({ value = [] as string[], disabled = false, onChange }: { value?: string[]; disabled?: boolean; onChange?: (v: string[]) => void }) {
  const [v, setV] = useState(value);
  return (
    <BuiltInRulePicker
      rules={rules}
      selected={v}
      disabled={disabled}
      onChange={(next) => {
        setV(next);
        onChange?.(next);
      }}
    />
  );
}

const rows = (list: string) =>
  within(screen.getByRole("list", { name: list }))
    .queryAllByRole("listitem")
    .map((li) => li.getAttribute("data-built-in"));
const enabled = () => rows("Enabled built-in rules");
const others = () => rows("Built-in rules not enabled");

describe("BuiltInRulePicker", () => {
  it("pins the enabled rules on top and lists the rest below, with their globs", () => {
    render(<Host value={["go"]} />);
    expect(enabled()).toEqual(["go"]);
    expect(others()).toEqual(["yaml", "ts_js"]);
    expect(screen.getByRole("heading", { name: "Enabled (1)" })).toBeInTheDocument();
    expect(screen.getByText("1 of 3 enabled")).toBeInTheDocument();
    const yaml = screen.getByRole("listitem", { name: "yaml" });
    expect(within(yaml).getByText("**/*.yaml")).toBeInTheDocument();
    expect(within(yaml).getByText("**/*.yml")).toBeInTheDocument();
    expect(screen.getByRole("checkbox", { name: "Built-in rule go" })).toBeChecked();
    expect(screen.getByRole("checkbox", { name: "Built-in rule yaml" })).not.toBeChecked();
    expect(document.body).not.toHaveTextContent(/chosen/i);
  });

  it("says when none is enabled", () => {
    render(<Host />);
    expect(enabled()).toEqual([]);
    expect(screen.getByText("No built-in rule is enabled.")).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "Enabled (0)" })).toBeInTheDocument();
  });

  it("moves a ticked rule up and an unticked one back down, keeping pergent's order", async () => {
    let last: string[] = [];
    render(<Host onChange={(v) => (last = v)} />);
    await userEvent.click(screen.getByRole("checkbox", { name: "Built-in rule ts_js" }));
    await userEvent.click(screen.getByRole("checkbox", { name: "Built-in rule yaml" }));
    expect(last).toEqual(["yaml", "ts_js"]);
    expect(enabled()).toEqual(["yaml", "ts_js"]);
    expect(others()).toEqual(["go"]);
    expect(screen.getByText("2 of 3 enabled")).toBeInTheDocument();
    await userEvent.click(within(screen.getByRole("list", { name: "Enabled built-in rules" })).getByRole("checkbox", { name: "Built-in rule yaml" }));
    expect(last).toEqual(["ts_js"]);
    expect(enabled()).toEqual(["ts_js"]);
    expect(others()).toEqual(["yaml", "go"]);
  });

  it("searches the rules not enabled by name and by glob, never hiding the enabled ones", async () => {
    let last: string[] = [];
    render(<Host value={["go"]} onChange={(v) => (last = v)} />);
    const search = screen.getByRole("searchbox", { name: "Search built-in rules" });
    await userEvent.type(search, "*.js");
    expect(others()).toEqual(["ts_js"]);
    expect(enabled()).toEqual(["go"]);
    await userEvent.click(screen.getByRole("checkbox", { name: "Built-in rule ts_js" }));
    expect(last).toEqual(["go", "ts_js"]);
    expect(enabled()).toEqual(["go", "ts_js"]);
    await userEvent.clear(search);
    await userEvent.type(search, "YAML");
    expect(others()).toEqual(["yaml"]);
    await userEvent.clear(search);
    await userEvent.type(search, "go");
    expect(enabled()).toEqual(["go", "ts_js"]);
    expect(screen.queryByRole("list", { name: "Built-in rules not enabled" })).not.toBeInTheDocument();
    expect(screen.getByText("No other built-in rule matches.")).toBeInTheDocument();
  });

  it("opens a row to show the rule's text", async () => {
    render(<Host value={["go"]} />);
    const go = screen.getByRole("listitem", { name: "go" });
    // The text is rendered markdown, which loads on its own: wait for it.
    const text = await within(go).findByText("Check the errors.", { selector: ".md p" });
    expect(text.closest("details")).not.toHaveAttribute("open");
    await userEvent.click(within(go).getByText("go"));
    expect(text.closest("details")).toHaveAttribute("open");
  });

  it("clears the list with Select none, which is off while none is enabled", async () => {
    let last: string[] = ["x"];
    render(<Host value={["go", "yaml"]} onChange={(v) => (last = v)} />);
    await userEvent.click(screen.getByRole("button", { name: "Select none" }));
    expect(last).toEqual([]);
    expect(screen.getByText("0 of 3 enabled")).toBeInTheDocument();
    expect(enabled()).toEqual([]);
    expect(screen.getByRole("button", { name: "Select none" })).toBeDisabled();
  });

  it("changes nothing while disabled", () => {
    render(<Host value={["go"]} disabled />);
    expect(screen.getByRole("checkbox", { name: "Built-in rule yaml" })).toBeDisabled();
    expect(screen.getByRole("checkbox", { name: "Built-in rule go" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Select none" })).toBeDisabled();
  });
});
