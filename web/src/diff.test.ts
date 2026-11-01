import { describe, expect, it } from "vitest";

import { lineDiff } from "./diff";

describe("lineDiff", () => {
  it("marks added and removed lines", () => {
    expect(lineDiff("a\nb\nc\n", "a\nx\nc\nd\n")).toEqual([
      { kind: "same", text: "a" },
      { kind: "del", text: "b" },
      { kind: "add", text: "x" },
      { kind: "same", text: "c" },
      { kind: "add", text: "d" },
    ]);
  });

  it("is all same for equal text, and empty for two empty texts", () => {
    expect(lineDiff("a\nb", "a\nb")).toEqual([
      { kind: "same", text: "a" },
      { kind: "same", text: "b" },
    ]);
    expect(lineDiff("", "")).toEqual([]);
  });

  it("keeps a blank line between two texts as a line", () => {
    expect(lineDiff("a\n\nb", "a\nb")).toEqual([
      { kind: "same", text: "a" },
      { kind: "del", text: "" },
      { kind: "same", text: "b" },
    ]);
  });

  it("falls back to remove-all then add-all for a very large change, and still ends right", () => {
    const a = Array.from({ length: 3000 }, (_, i) => `a${i}`).join("\n");
    const b = Array.from({ length: 3000 }, (_, i) => `b${i}`).join("\n");
    const d = lineDiff(a, b);
    expect(d).toHaveLength(6000);
    expect(d.filter((l) => l.kind === "del")).toHaveLength(3000);
    expect(d.filter((l) => l.kind === "add")).toHaveLength(3000);
    expect(d[0]).toEqual({ kind: "del", text: "a0" });
  });
});
