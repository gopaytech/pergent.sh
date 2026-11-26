import { fireEvent, render, screen } from "@testing-library/react";
import { MemoryRouter, useLocation } from "react-router";
import { describe, expect, it } from "vitest";

import { compareRows, SortHeader, useTableSort } from "./TableSort";

const keys = ["name", "reviews"] as const;

function Probe() {
  const [sort, onSort] = useTableSort(keys, "name", ["reviews"]);
  const loc = useLocation();
  return (
    <>
      <table>
        <thead>
          <tr>
            <SortHeader label="Name" sortKey="name" sort={sort} onSort={onSort} />
            <SortHeader label="Reviews" sortKey="reviews" sort={sort} onSort={onSort} num />
          </tr>
        </thead>
      </table>
      <output data-testid="search">{loc.search}</output>
    </>
  );
}

const show = (at: string) =>
  render(
    <MemoryRouter initialEntries={[at]}>
      <Probe />
    </MemoryRouter>,
  );
const header = (name: string) => screen.getByRole("columnheader", { name });
const search = () => screen.getByTestId("search").textContent;

describe("useTableSort and SortHeader", () => {
  it("starts on the fallback, ascending", () => {
    show("/x");
    expect(header("Name")).toHaveAttribute("aria-sort", "ascending");
    expect(header("Reviews")).toHaveAttribute("aria-sort", "none");
  });

  it("sorts a number column descending first, then flips", () => {
    show("/x");
    fireEvent.click(screen.getByRole("button", { name: /Reviews/ }));
    expect(search()).toBe("?sort=reviews-desc");
    expect(header("Reviews")).toHaveAttribute("aria-sort", "descending");
    fireEvent.click(screen.getByRole("button", { name: /Reviews/ }));
    expect(search()).toBe("?sort=reviews");
    expect(header("Reviews")).toHaveAttribute("aria-sort", "ascending");
  });

  it("drops the sort param for the fallback ascending", () => {
    show("/x?sort=reviews");
    fireEvent.click(screen.getByRole("button", { name: /Name/ }));
    expect(search()).toBe("");
    expect(header("Name")).toHaveAttribute("aria-sort", "ascending");
  });

  it("flips the fallback to descending", () => {
    show("/x");
    fireEvent.click(screen.getByRole("button", { name: /Name/ }));
    expect(search()).toBe("?sort=name-desc");
  });

  it.each(["bogus", "-desc", "bogus-desc"])("treats ?sort=%s as the fallback", (v) => {
    show(`/x?sort=${v}`);
    expect(header("Name")).toHaveAttribute("aria-sort", "ascending");
    expect(header("Reviews")).toHaveAttribute("aria-sort", "none");
  });

  it("keeps other params when it writes", () => {
    show("/x?q=pay&team=devs");
    fireEvent.click(screen.getByRole("button", { name: /Reviews/ }));
    const p = new URLSearchParams(search() ?? "");
    expect(p.get("q")).toBe("pay");
    expect(p.get("team")).toBe("devs");
    expect(p.get("sort")).toBe("reviews-desc");
  });
});

describe("compareRows", () => {
  type R = { n: string; v: number };
  const val = (r: R) => r.v;
  const name = (r: R) => r.n;
  it("orders equal values by name ascending, even when descending", () => {
    const rows: R[] = [
      { n: "b", v: 1 },
      { n: "a", v: 1 },
      { n: "c", v: 2 },
    ];
    expect([...rows].sort((x, y) => compareRows(x, y, val, false, name)).map(name)).toEqual(["a", "b", "c"]);
    expect([...rows].sort((x, y) => compareRows(x, y, val, true, name)).map(name)).toEqual(["c", "a", "b"]);
  });
  it("compares text values", () => {
    const rows = [
      { n: "x", v: 0 },
      { n: "y", v: 0 },
    ];
    const text = (r: R) => r.n;
    expect(compareRows(rows[0], rows[1], text, true, name)).toBeGreaterThan(0);
  });
});
