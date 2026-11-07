import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState } from "react";
import { describe, expect, it, vi } from "vitest";

import CommentLayoutEditor from "./CommentLayoutEditor";

const layout = ["summary:open", "findings:collapsed", "dismissed:collapsed", "checks:open", "files:collapsed"];

const rowNames = () => within(screen.getByRole("list", { name: "Comment layout" })).getAllByRole("listitem").map((r) => within(r).getByText(/^(Summary|Findings|Dismissed|Checks|Files)$/).textContent);

// Holds the value the way a page does, and shows what it was last given.
function Harness({ start, defaults, onValue }: { start: string[] | null; defaults?: string[]; onValue?: (v: string[] | null) => void }) {
  const [value, setValue] = useState(start);
  return (
    <>
      <CommentLayoutEditor
        value={value}
        defaults={defaults}
        onChange={(v) => {
          setValue(v);
          onValue?.(v);
        }}
      />
      <output data-testid="value">{JSON.stringify(value)}</output>
    </>
  );
}

describe("CommentLayoutEditor", () => {
  it("renders the five blocks in the order of the value, with plain labels", () => {
    render(<CommentLayoutEditor value={layout} onChange={vi.fn()} />);
    expect(rowNames()).toEqual(["Summary", "Findings", "Dismissed", "Checks", "Files"]);
  });

  it("moves a row up and calls onChange with the new order", async () => {
    const onChange = vi.fn();
    render(<CommentLayoutEditor value={layout} onChange={onChange} />);
    await userEvent.click(screen.getByRole("button", { name: "Move Dismissed up" }));
    expect(onChange).toHaveBeenCalledWith(["summary:open", "dismissed:collapsed", "findings:collapsed", "checks:open", "files:collapsed"]);
    await userEvent.click(screen.getByRole("button", { name: "Move Summary down" }));
    expect(onChange).toHaveBeenLastCalledWith(["findings:collapsed", "summary:open", "dismissed:collapsed", "checks:open", "files:collapsed"]);
  });

  it("has no up button on the first row and no down button on the last shown row", () => {
    render(<CommentLayoutEditor value={layout} onChange={vi.fn()} />);
    expect(screen.getByRole("button", { name: "Move Summary up" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Move Files down" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Move Files up" })).toBeEnabled();
  });

  it("gives Findings no show box, and says blocking findings stay open", () => {
    render(<CommentLayoutEditor value={layout} onChange={vi.fn()} />);
    expect(screen.queryByRole("checkbox", { name: "Show Findings" })).not.toBeInTheDocument();
    expect(screen.getByRole("checkbox", { name: "Show Summary" })).toBeChecked();
    expect(screen.getByText(/Blocking findings always stay open/)).toBeInTheDocument();
  });

  it("removes an unticked block from the value and keeps its row at the end", async () => {
    render(<Harness start={layout} />);
    await userEvent.click(screen.getByRole("checkbox", { name: "Show Summary" }));
    expect(JSON.parse(screen.getByTestId("value").textContent ?? "")).toEqual(["findings:collapsed", "dismissed:collapsed", "checks:open", "files:collapsed"]);
    expect(rowNames()).toEqual(["Findings", "Dismissed", "Checks", "Files", "Summary"]);
    expect(screen.getByRole("checkbox", { name: "Show Summary" })).not.toBeChecked();
  });

  it("gives a block back the state it had when it is ticked again", async () => {
    render(<Harness start={layout} />);
    await userEvent.click(screen.getByRole("checkbox", { name: "Show Dismissed" }));
    expect(screen.getByRole("combobox", { name: "Dismissed state" })).toHaveValue("collapsed");
    expect(JSON.parse(screen.getByTestId("value").textContent ?? "")).not.toContain("dismissed:collapsed");
    await userEvent.click(screen.getByRole("checkbox", { name: "Show Dismissed" }));
    expect(JSON.parse(screen.getByTestId("value").textContent ?? "")).toEqual(["summary:open", "findings:collapsed", "checks:open", "files:collapsed", "dismissed:collapsed"]);
  });

  it("shows a block left out of the value as an unticked row, and ticking it adds it last", async () => {
    render(<Harness start={["findings:open", "checks:collapsed"]} defaults={layout} />);
    expect(rowNames()).toEqual(["Findings", "Checks", "Summary", "Dismissed", "Files"]);
    expect(screen.getByRole("checkbox", { name: "Show Dismissed" })).not.toBeChecked();
    expect(screen.getByRole("combobox", { name: "Dismissed state" })).toHaveValue("collapsed");
    await userEvent.click(screen.getByRole("checkbox", { name: "Show Dismissed" }));
    expect(JSON.parse(screen.getByTestId("value").textContent ?? "")).toEqual(["findings:open", "checks:collapsed", "dismissed:collapsed"]);
  });

  it("writes block:collapsed when the state select says collapsed", async () => {
    const onChange = vi.fn();
    render(<CommentLayoutEditor value={layout} onChange={onChange} />);
    await userEvent.selectOptions(screen.getByRole("combobox", { name: "Checks state" }), "collapsed");
    expect(onChange).toHaveBeenCalledWith(["summary:open", "findings:collapsed", "dismissed:collapsed", "checks:collapsed", "files:collapsed"]);
    await userEvent.selectOptions(screen.getByRole("combobox", { name: "Findings state" }), "open");
    expect(onChange).toHaveBeenLastCalledWith(["summary:open", "findings:open", "dismissed:collapsed", "checks:open", "files:collapsed"]);
  });

  it("calls onChange(null) from Reset to default", async () => {
    const onChange = vi.fn();
    render(<CommentLayoutEditor value={["findings:open"]} onChange={onChange} />);
    await userEvent.click(screen.getByRole("button", { name: "Reset to default" }));
    expect(onChange).toHaveBeenCalledWith(null);
  });

  it("shows the default list for a null value, starts an edit from it, and has nothing to reset", async () => {
    const onChange = vi.fn();
    render(<CommentLayoutEditor value={null} defaults={layout} onChange={onChange} />);
    expect(rowNames()).toEqual(["Summary", "Findings", "Dismissed", "Checks", "Files"]);
    expect(screen.getByRole("button", { name: "Reset to default" })).toBeDisabled();
    await userEvent.click(screen.getByRole("button", { name: "Move Files up" }));
    expect(onChange).toHaveBeenCalledWith(["summary:open", "findings:collapsed", "dismissed:collapsed", "files:collapsed", "checks:open"]);
  });

  it("says the default applies when it has a null value and no default to show", () => {
    render(<CommentLayoutEditor value={null} onChange={vi.fn()} />);
    expect(screen.getByText(/follows the default layout/)).toBeInTheDocument();
    expect(screen.queryByRole("list", { name: "Comment layout" })).not.toBeInTheDocument();
  });

  it("changes nothing when disabled", () => {
    render(<CommentLayoutEditor value={layout} onChange={vi.fn()} disabled />);
    expect(screen.getByRole("checkbox", { name: "Show Summary" })).toBeDisabled();
    expect(screen.getByRole("combobox", { name: "Checks state" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Move Checks up" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Reset to default" })).toBeDisabled();
  });
});
