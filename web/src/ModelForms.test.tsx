import { describe, expect, it } from "vitest";

import { parseTokens, windowText } from "./ModelForms";

describe("parseTokens", () => {
  it.each([
    ["1,000,000", 1000000],
    [" 64 000 ", 64000],
    ["1_000", 1000],
    ["", null],
    ["  ", null],
    ["1M", undefined],
    ["1.5", undefined],
    ["-5", undefined],
  ])("reads %j as %j", (text, want) => {
    expect(parseTokens(text)).toBe(want);
  });
});

describe("windowText", () => {
  it.each([
    [1_000_000, 64_000, "1M / 64k"],
    [200_000, null, "200k / –"],
    [null, 64_000, "– / 64k"],
    [null, null, ""],
  ])("shows %j and %j as %j", (contextWindow, maxOutput, want) => {
    expect(windowText(contextWindow, maxOutput)).toBe(want);
  });
});
