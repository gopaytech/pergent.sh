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

const shown = () =>
  within(screen.getByRole("list", { name: "Built-in rules" }))
    .getAllByRole("listitem")
    .map((li) => li.getAttribute("data-built-in"));

describe("BuiltInRulePicker", () => {
  it("lists every rule with its globs and counts the chosen ones", () => {
    render(<Host value={["go"]} />);
    expect(shown()).toEqual(["yaml", "go", "ts_js"]);
    expect(screen.getByText("1 of 3 chosen")).toBeInTheDocument();
    const yaml = screen.getByRole("listitem", { name: "yaml" });
    expect(within(yaml).getByText("**/*.yaml")).toBeInTheDocument();
    expect(within(yaml).getByText("**/*.yml")).toBeInTheDocument();
    expect(screen.getByRole("checkbox", { name: "Built-in rule go" })).toBeChecked();
    expect(screen.getByRole("checkbox", { name: "Built-in rule yaml" })).not.toBeChecked();
  });

  it("ticks and unticks rules, keeping pergent's order", async () => {
    let last: string[] = [];
    render(<Host onChange={(v) => (last = v)} />);
    await userEvent.click(screen.getByRole("checkbox", { name: "Built-in rule ts_js" }));
    await userEvent.click(screen.getByRole("checkbox", { name: "Built-in rule yaml" }));
    expect(last).toEqual(["yaml", "ts_js"]);
    expect(screen.getByText("2 of 3 chosen")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("checkbox", { name: "Built-in rule yaml" }));
    expect(last).toEqual(["ts_js"]);
  });

  it("searches by name and by glob, keeping hidden choices", async () => {
    let last: string[] = [];
    render(<Host value={["go"]} onChange={(v) => (last = v)} />);
    const search = screen.getByRole("searchbox", { name: "Search built-in rules" });
    await userEvent.type(search, "*.js");
    expect(shown()).toEqual(["ts_js"]);
    await userEvent.click(screen.getByRole("checkbox", { name: "Built-in rule ts_js" }));
    expect(last).toEqual(["go", "ts_js"]);
    await userEvent.clear(search);
    await userEvent.type(search, "YAML");
    expect(shown()).toEqual(["yaml"]);
    await userEvent.clear(search);
    await userEvent.type(search, "nothing");
    expect(screen.queryByRole("list", { name: "Built-in rules" })).not.toBeInTheDocument();
    expect(screen.getByText("No built-in rule matches.")).toBeInTheDocument();
  });

  it("selects none with one button, which is off while none is chosen", async () => {
    let last: string[] = ["x"];
    render(<Host value={["go", "yaml"]} onChange={(v) => (last = v)} />);
    await userEvent.click(screen.getByRole("button", { name: "Select none" }));
    expect(last).toEqual([]);
    expect(screen.getByText("0 of 3 chosen")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Select none" })).toBeDisabled();
  });

  it("changes nothing while disabled", () => {
    render(<Host value={["go"]} disabled />);
    expect(screen.getByRole("checkbox", { name: "Built-in rule yaml" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Select none" })).toBeDisabled();
  });
});
