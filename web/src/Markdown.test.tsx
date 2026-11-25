import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import Markdown, { splitFrontmatter } from "./Markdown";

describe("splitFrontmatter", () => {
  it("takes a leading --- block off, and leaves text with none as it is", () => {
    expect(splitFrontmatter("---\nname: a\ndescription: b\n---\n# Body\n")).toEqual({ front: "name: a\ndescription: b", body: "# Body\n" });
    expect(splitFrontmatter("# Body\n---\nnot front\n---\n")).toEqual({ front: "", body: "# Body\n---\nnot front\n---\n" });
  });
});

describe("Markdown", () => {
  it("renders headings, lists, code and tables, with the front matter apart", () => {
    const text = "---\nname: go-style\n---\n# Go style\n\n- Wrap errors\n- Use `ctx`\n\n| a | b |\n|---|---|\n| 1 | 2 |\n";
    const { container } = render(<Markdown text={text} />);
    expect(screen.getByRole("heading", { name: "Go style" })).toBeInTheDocument();
    expect(screen.getAllByRole("listitem")).toHaveLength(2);
    expect(screen.getByText("ctx").tagName).toBe("CODE");
    expect(screen.getByRole("table")).toBeInTheDocument();
    expect(screen.getByLabelText("Front matter")).toHaveTextContent("name: go-style");
    expect(container.querySelector(".md")).not.toHaveTextContent("name: go-style");
  });

  it("writes HTML out as text and never makes an element of it", () => {
    const { container } = render(<Markdown text={'Hi <img src=x onerror="alert(1)"> <b>bold</b>'} />);
    expect(container.querySelector("img")).toBeNull();
    expect(container.querySelector("b")).toBeNull();
    expect(container).toHaveTextContent('<img src=x onerror="alert(1)">');
  });

  it("opens http(s) links in a new tab, and makes no link of another scheme", () => {
    render(<Markdown text={"[docs](https://example.com/a) and [bad](javascript:alert(1))"} />);
    const docs = screen.getByRole("link", { name: "docs" });
    expect(docs).toHaveAttribute("href", "https://example.com/a");
    expect(docs).toHaveAttribute("target", "_blank");
    expect(docs).toHaveAttribute("rel", "noopener noreferrer");
    expect(screen.queryByRole("link", { name: "bad" })).not.toBeInTheDocument();
    expect(screen.getByText("bad")).toBeInTheDocument();
  });

  it("shows an image as a link to it, never loads it", () => {
    const { container } = render(<Markdown text={"![diagram](https://example.com/d.png)"} />);
    expect(container.querySelector("img")).toBeNull();
    expect(screen.getByRole("link", { name: "[image: diagram]" })).toHaveAttribute("href", "https://example.com/d.png");
  });

  it("shows the text as written with Show source, and back", async () => {
    render(<Markdown text={"# Title\n"} />);
    await userEvent.click(screen.getByRole("button", { name: "Show source" }));
    expect(screen.getByText("# Title").tagName).toBe("PRE");
    await userEvent.click(screen.getByRole("button", { name: "Show rendered" }));
    expect(screen.getByRole("heading", { name: "Title" })).toBeInTheDocument();
  });
});
