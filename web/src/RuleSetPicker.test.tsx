import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState } from "react";
import { describe, expect, it } from "vitest";

import RuleSetPicker from "./RuleSetPicker";
import { ruleSets } from "./test/fixtures";

// Host keeps the state the page keeps, so a change shows in the picker.
function Host({ value = [] as string[], onChange }: { value?: string[]; onChange?: (v: string[]) => void }) {
  const [v, setV] = useState(value);
  return (
    <RuleSetPicker
      label="Rule sets"
      library={[...ruleSets, { ...ruleSets[0], name: "py" }]}
      selected={v}
      disabled={false}
      onChange={(next) => {
        setV(next);
        onChange?.(next);
      }}
    />
  );
}

const order = () =>
  within(screen.getByRole("list", { name: "Rule sets in match order" }))
    .getAllByRole("listitem")
    .map((li) => li.getAttribute("data-rule-set"));

describe("RuleSetPicker", () => {
  it("adds sets at the end, moves them and removes them, keeping the order", async () => {
    let last: string[] = [];
    render(<Host onChange={(v) => (last = v)} />);
    const add = screen.getByRole("combobox", { name: "Add a rule set" });
    await userEvent.selectOptions(add, "go-api");
    await userEvent.selectOptions(add, "py");
    expect(order()).toEqual(["go-api", "py"]);
    await userEvent.click(screen.getByRole("button", { name: "Move py up" }));
    expect(order()).toEqual(["py", "go-api"]);
    await userEvent.click(screen.getByRole("button", { name: "Remove go-api" }));
    expect(order()).toEqual(["py"]);
    expect(last).toEqual(["py"]);
  });

  it("offers only library sets not chosen yet, never a config-file set", () => {
    render(<Host value={["go-api"]} />);
    const options = within(screen.getByRole("combobox", { name: "Add a rule set" }))
      .getAllByRole("option")
      .map((o) => o.textContent);
    expect(options).toEqual(["Add a rule set…", "py"]);
  });

  it("marks a chosen name the library no longer has", () => {
    render(<Host value={["gone"]} />);
    expect(screen.getByText("gone").closest("li")).toHaveTextContent("not in the library");
  });
});
