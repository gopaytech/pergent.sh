import { describe, expect, it } from "vitest";

import { tok } from "../test/fixtures";
import { cachedPercent, dash, output, partsText, prompt, total } from "./usageParts";

const five = tok(42, 2_500, { cacheRead: 525_000, cacheWrite: 75_000, reasoning: 2_000, unknown: 0 });

describe("usage parts", () => {
  it("reads total, prompt and output from the server's numbers", () => {
    expect([total(five), prompt(five), output(five)]).toEqual([604_542, 600_042, 4_500]);
  });

  it.each([
    ["cache read of the prompt, whole percent", five, "87%"],
    ["no prompt", tok(0, 0), dash],
    ["unknown with no cache and no reasoning", tok(100, 5, { cacheRead: 0, cacheWrite: 0, reasoning: 0, unknown: 3 }), dash],
    ["unknown but some cache read", tok(50, 5, { cacheRead: 50, cacheWrite: 0, reasoning: 0, unknown: 3 }), "50%"],
  ])("cachedPercent: %s", (_name, t, want) => {
    expect(cachedPercent(t)).toBe(want);
  });

  it("partsText names all 5 parts, and the unknown skill runs", () => {
    expect(partsText(five)).toBe("input 42 · cache read 525k · cache write 75k · output 2.5k · reasoning 2k");
    expect(partsText(tok(100, 5, { cacheRead: 0, cacheWrite: 0, reasoning: 0, unknown: 1 }))).toBe(
      "input 100 · cache read 0 · cache write 0 · output 5 · reasoning 0 · unknown for 1 skill run",
    );
  });
});
