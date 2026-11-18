import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState } from "react";
import { describe, expect, it, vi } from "vitest";

import type { CIJobs } from "./api";
import CIJobsEditor, { providerWarning, unknownNameWarning } from "./CIJobsEditor";

const status = { name: "build", read: "status" as const, blocksApproval: false };
const log = { name: "test", read: "log" as const, blocksApproval: true };

// Host keeps the state the page keeps, so a change shows in the editor.
function Host({
  value = { wait: "30m", jobs: [] },
  names = ["build", "test"],
  github = false,
  onChange,
  disabled,
}: {
  value?: CIJobs;
  names?: string[] | null;
  github?: boolean;
  onChange?: (v: CIJobs) => void;
  disabled?: boolean;
}) {
  const [v, setV] = useState(value);
  return (
    <CIJobsEditor
      value={v}
      names={names}
      github={github}
      disabled={disabled}
      onChange={(next) => {
        setV(next);
        onChange?.(next);
      }}
    />
  );
}

const rows = () => within(screen.getByRole("list", { name: "CI jobs" })).queryAllByRole("listitem");

describe("CIJobsEditor", () => {
  it("says there are no jobs yet, with the wait", () => {
    render(<Host />);
    expect(rows()).toHaveLength(0);
    expect(screen.getByText("No CI job yet.")).toBeInTheDocument();
    expect(screen.getByRole("textbox", { name: "How long to wait for CI" })).toHaveValue("30m");
  });

  it("adds a job, edits it and removes it", async () => {
    const onChange = vi.fn();
    render(<Host onChange={onChange} />);
    await userEvent.click(screen.getByRole("button", { name: "Add a job" }));
    expect(onChange).toHaveBeenLastCalledWith({ wait: "30m", jobs: [{ name: "", read: "status", blocksApproval: false }] });

    const row = rows()[0];
    await userEvent.type(within(row).getByRole("combobox", { name: "Job name" }), "lint");
    await userEvent.selectOptions(within(row).getByRole("combobox", { name: "Read" }), "log");
    await userEvent.click(within(row).getByRole("switch", { name: "Blocks approval" }));
    expect(onChange).toHaveBeenLastCalledWith({ wait: "30m", jobs: [{ name: "lint", read: "log", blocksApproval: true }] });

    await userEvent.clear(screen.getByRole("textbox", { name: "How long to wait for CI" }));
    await userEvent.type(screen.getByRole("textbox", { name: "How long to wait for CI" }), "1h");
    expect(onChange).toHaveBeenLastCalledWith({ wait: "1h", jobs: [{ name: "lint", read: "log", blocksApproval: true }] });

    await userEvent.click(within(rows()[0]).getByRole("button", { name: "Remove lint" }));
    expect(rows()).toHaveLength(0);
    expect(onChange).toHaveBeenLastCalledWith({ wait: "1h", jobs: [] });
  });

  it("stops at 10 jobs", () => {
    const jobs = Array.from({ length: 10 }, (_, i) => ({ ...status, name: `job-${i}` }));
    render(<Host value={{ wait: "30m", jobs }} />);
    expect(screen.getByRole("button", { name: "Add a job" })).toBeDisabled();
    expect(screen.getByText(/at most 10 jobs/)).toBeInTheDocument();
  });

  it("asks for a report path for read: report, and for the artifact on GitHub only", async () => {
    const onChange = vi.fn();
    const { unmount } = render(<Host value={{ wait: "30m", jobs: [log] }} onChange={onChange} />);
    const row = rows()[0];
    expect(within(row).queryByRole("textbox", { name: "Report path" })).not.toBeInTheDocument();
    await userEvent.selectOptions(within(row).getByRole("combobox", { name: "Read" }), "report");
    expect(within(row).queryByRole("textbox", { name: "Report artifact" })).not.toBeInTheDocument();
    await userEvent.type(within(row).getByRole("textbox", { name: "Report path" }), "out.json");
    expect(onChange).toHaveBeenLastCalledWith({
      wait: "30m",
      jobs: [{ name: "test", read: "report", report: { path: "out.json" }, blocksApproval: true }],
    });
    // Back to log: the report goes.
    await userEvent.selectOptions(within(row).getByRole("combobox", { name: "Read" }), "log");
    expect(onChange).toHaveBeenLastCalledWith({ wait: "30m", jobs: [log] });
    unmount();

    render(<Host github value={{ wait: "30m", jobs: [{ ...log, read: "report", report: { path: "" } }] }} onChange={onChange} />);
    await userEvent.type(within(rows()[0]).getByRole("textbox", { name: "Report artifact" }), "scan");
    await userEvent.type(within(rows()[0]).getByRole("textbox", { name: "Report path" }), "a.json");
    expect(onChange).toHaveBeenLastCalledWith({
      wait: "30m",
      jobs: [{ ...log, read: "report", report: { artifact: "scan", path: "a.json" } }],
    });
  });

  it("lists the names it was given as suggestions and takes free text", async () => {
    const onChange = vi.fn();
    const { container } = render(<Host names={["build", "test (1.22)"]} value={{ wait: "30m", jobs: [{ ...status, name: "" }] }} onChange={onChange} />);
    const options = [...container.querySelectorAll("datalist option")].map((o) => o.getAttribute("value"));
    expect(options).toEqual(["build", "test (1.22)"]);
    const box = within(rows()[0]).getByRole("combobox", { name: "Job name" });
    expect(box).toHaveAttribute("list", container.querySelector("datalist")?.id);
    await userEvent.type(box, "deploy-docs");
    expect(onChange).toHaveBeenLastCalledWith({ wait: "30m", jobs: [{ ...status, name: "deploy-docs" }] });
  });

  it("warns about a name not seen in the recent runs, and not about one that was", () => {
    render(<Host names={["build"]} value={{ wait: "30m", jobs: [status, { ...status, name: "bild" }] }} />);
    expect(within(rows()[0]).queryByText(unknownNameWarning)).not.toBeInTheDocument();
    expect(within(rows()[1]).getByText(unknownNameWarning)).toBeInTheDocument();
    expect(unknownNameWarning).toBe("Not seen in the last 10 runs. Check the name.");
  });

  it("does not warn when the names could not be read, or the name is empty", () => {
    const { unmount } = render(<Host names={null} value={{ wait: "30m", jobs: [{ ...status, name: "bild" }] }} />);
    expect(screen.queryByText(unknownNameWarning)).not.toBeInTheDocument();
    unmount();
    render(<Host names={["build"]} value={{ wait: "30m", jobs: [{ ...status, name: "" }] }} />);
    expect(screen.queryByText(unknownNameWarning)).not.toBeInTheDocument();
  });

  it("warns that CI output goes to the model provider beside log and report, and not beside status", () => {
    render(
      <Host
        value={{
          wait: "30m",
          jobs: [status, log, { name: "scan", read: "report", report: { path: "s.json" }, blocksApproval: false }],
        }}
      />,
    );
    expect(within(rows()[0]).queryByText(providerWarning)).not.toBeInTheDocument();
    expect(within(rows()[1]).getByText(providerWarning)).toBeInTheDocument();
    expect(within(rows()[2]).getByText(providerWarning)).toBeInTheDocument();
    expect(providerWarning).toBe("CI output goes to the model provider. Secrets are removed first, but only the shapes pergent.sh knows.");
  });

  it("is read-only when disabled", () => {
    render(<Host disabled value={{ wait: "30m", jobs: [log] }} />);
    expect(screen.getByRole("textbox", { name: "How long to wait for CI" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Add a job" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Remove test" })).toBeDisabled();
    expect(screen.getByRole("combobox", { name: "Job name" })).toBeDisabled();
    expect(screen.getByRole("switch", { name: "Blocks approval" })).toBeDisabled();
  });
});
