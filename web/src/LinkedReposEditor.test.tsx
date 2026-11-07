import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState } from "react";
import { describe, expect, it, vi } from "vitest";

import type { Link } from "./api";
import LinkedReposEditor, { tidyLinks, type LinkableState } from "./LinkedReposEditor";
import { linkable } from "./test/fixtures";

const ready: LinkableState = { status: "ok", view: linkable };
const api: Link = { target: "acme/api", watched: ["render/**"], canBlock: true };
const lib: Link = { target: "acme/lib", watched: [], canBlock: false };

// Host keeps the state the page keeps, so a change shows in the editor.
function Host({
  enabled = false,
  links = [],
  state = ready,
  onLinks,
  onEnabled,
  disabled,
}: {
  enabled?: boolean;
  links?: Link[];
  state?: LinkableState;
  onLinks?: (l: Link[]) => void;
  onEnabled?: (on: boolean) => void;
  disabled?: boolean;
}) {
  const [on, setOn] = useState(enabled);
  const [list, setList] = useState(links);
  return (
    <LinkedReposEditor
      enabled={on}
      onEnabled={(v) => {
        setOn(v);
        onEnabled?.(v);
      }}
      links={list}
      onLinks={(l) => {
        setList(l);
        onLinks?.(l);
      }}
      linkable={state}
      onSearch={vi.fn()}
      disabled={disabled}
    />
  );
}

describe("LinkedReposEditor", () => {
  it("starts with the switch, off, and says what off means", () => {
    render(<Host />);
    const sw = screen.getByRole("switch", { name: "Linked repositories" });
    expect(sw).not.toBeChecked();
    // The switch is the first control.
    expect(screen.getAllByRole("switch")[0]).toBe(sw);
    expect(screen.getByText(/Off by default/)).toBeInTheDocument();
    expect(screen.queryByText(/ignored until/)).not.toBeInTheDocument();
  });

  it("turns on and off", async () => {
    const onEnabled = vi.fn();
    render(<Host onEnabled={onEnabled} />);
    await userEvent.click(screen.getByRole("switch", { name: "Linked repositories" }));
    expect(onEnabled).toHaveBeenLastCalledWith(true);
    expect(screen.getByRole("switch", { name: "Linked repositories" })).toBeChecked();
  });

  it("keeps the links editable while off, and says they are ignored until it is on", async () => {
    const onLinks = vi.fn();
    render(<Host links={[api]} onLinks={onLinks} />);
    expect(screen.getByText(/ignored until/)).toBeInTheDocument();
    const box = screen.getByRole("textbox", { name: "Watched paths of acme/api" });
    expect(box).toBeEnabled();
    await userEvent.type(box, "x");
    expect(onLinks).toHaveBeenCalled();
    // On: the note goes.
    await userEvent.click(screen.getByRole("switch", { name: "Linked repositories" }));
    expect(screen.queryByText(/ignored until/)).not.toBeInTheDocument();
  });

  it("adds a target from the list, with Can block on and no watched paths", async () => {
    const onLinks = vi.fn();
    render(<Host onLinks={onLinks} />);
    const list = screen.getByRole("list", { name: "Repositories to link" });
    await userEvent.click(within(list).getByRole("button", { name: "Add acme/api" }));
    expect(onLinks).toHaveBeenLastCalledWith([{ target: "acme/api", watched: [], canBlock: true }]);
    // It is a row now.
    expect(screen.getByRole("textbox", { name: "Watched paths of acme/api" })).toHaveValue("");
    expect(screen.getByRole("switch", { name: "Can block acme/api" })).toBeChecked();
    // And no longer offered.
    expect(within(list).queryByRole("button", { name: "Add acme/api" })).not.toBeInTheDocument();
    expect(within(list).getByText("added")).toBeInTheDocument();
  });

  it("edits the watched paths, one glob a line", async () => {
    const onLinks = vi.fn();
    render(<Host links={[api]} onLinks={onLinks} />);
    const box = screen.getByRole("textbox", { name: "Watched paths of acme/api" });
    expect(box).toHaveValue("render/**");
    await userEvent.type(box, "{Enter}scope/**");
    expect(onLinks).toHaveBeenLastCalledWith([{ target: "acme/api", watched: ["render/**", "scope/**"], canBlock: true }]);
    expect(box).toHaveValue("render/**\nscope/**");
  });

  it("switches Can block, and says an off link only suggests", async () => {
    const onLinks = vi.fn();
    render(<Host links={[api]} onLinks={onLinks} />);
    await userEvent.click(screen.getByRole("switch", { name: "Can block acme/api" }));
    expect(onLinks).toHaveBeenLastCalledWith([{ target: "acme/api", watched: ["render/**"], canBlock: false }]);
    expect(screen.getByText(/only ever suggestions/)).toBeInTheDocument();
  });

  it("removes a link and offers it again", async () => {
    const onLinks = vi.fn();
    render(<Host links={[api, lib]} onLinks={onLinks} />);
    await userEvent.click(screen.getByRole("button", { name: "Remove acme/api" }));
    expect(onLinks).toHaveBeenLastCalledWith([lib]);
    expect(screen.queryByRole("textbox", { name: "Watched paths of acme/api" })).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Add acme/api" })).toBeInTheDocument();
  });

  it("stops at three links", async () => {
    const three: Link[] = ["acme/a", "acme/b", "acme/c"].map((target) => ({ target, watched: [], canBlock: true }));
    render(<Host links={three} />);
    expect(screen.queryByRole("list", { name: "Repositories to link" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /^Add / })).not.toBeInTheDocument();
    expect(screen.getByText(/at most 3 linked repositories/)).toBeInTheDocument();
    // Removing one brings the list back.
    await userEvent.click(screen.getByRole("button", { name: "Remove acme/b" }));
    expect(screen.getByRole("button", { name: "Add acme/api" })).toBeInTheDocument();
  });

  it("stops at three while adding", async () => {
    render(<Host links={[{ target: "acme/a", watched: [], canBlock: true }, { target: "acme/b", watched: [], canBlock: true }]} />);
    await userEvent.click(screen.getByRole("button", { name: "Add acme/api" }));
    expect(screen.queryByRole("button", { name: /^Add / })).not.toBeInTheDocument();
    expect(screen.getAllByRole("button", { name: /^Remove / })).toHaveLength(3);
  });

  it("shows why a repository can't be linked, with no way to add it", () => {
    render(<Host />);
    const list = screen.getByRole("list", { name: "Repositories to link" });
    const other = within(list).getByText("acme/other-app").closest("[role=listitem]") as HTMLElement;
    expect(other).toHaveTextContent("it is in another installation of the review App");
    expect(within(other).queryByRole("button")).not.toBeInTheDocument();
    const ghost = within(list).getByText("acme/ghost").closest("[role=listitem]") as HTMLElement;
    expect(ghost).toHaveTextContent("the pergent review App can't read it");
    expect(within(ghost).queryByRole("button")).not.toBeInTheDocument();
  });

  it("says a repository that was not checked is checked when saved, and still offers it", () => {
    render(<Host />);
    const slow = within(screen.getByRole("list", { name: "Repositories to link" })).getByText("acme/slow").closest("[role=listitem]") as HTMLElement;
    expect(slow).toHaveTextContent("checked when you save");
    expect(within(slow).getByRole("button", { name: "Add acme/slow" })).toBeInTheDocument();
  });

  it("shows each repository's visibility", () => {
    render(<Host />);
    const api = within(screen.getByRole("list", { name: "Repositories to link" })).getByText("acme/api").closest("[role=listitem]") as HTMLElement;
    expect(api).toHaveTextContent("public");
  });

  it("says what is happening while the list loads, and when it fails", () => {
    const { rerender } = render(<Host state={{ status: "loading" }} />);
    expect(screen.getByText("Loading…")).toBeInTheDocument();
    rerender(<Host state={{ status: "error", error: new Error("boom") }} />);
    expect(screen.getByRole("alert")).toBeInTheDocument();
  });

  it("says when the list was cut", () => {
    render(<Host state={{ status: "ok", view: { ...linkable, truncated: true } }} />);
    expect(screen.getByText(/first 200/)).toBeInTheDocument();
  });

  it("locks every control when disabled", () => {
    render(<Host links={[api]} disabled />);
    expect(screen.getByRole("switch", { name: "Linked repositories" })).toBeDisabled();
    expect(screen.getByRole("textbox", { name: "Watched paths of acme/api" })).toBeDisabled();
    expect(screen.getByRole("switch", { name: "Can block acme/api" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Remove acme/api" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Add acme/lib" })).toBeDisabled();
  });
});

describe("tidyLinks", () => {
  it("trims the globs and drops blank lines", () => {
    expect(tidyLinks([{ target: "acme/api", watched: [" a/** ", "", "  ", "b/**"], canBlock: false }])).toEqual([
      { target: "acme/api", watched: ["a/**", "b/**"], canBlock: false },
    ]);
  });
});
