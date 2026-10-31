import { screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { fakeApi } from "../test/fakeApi";
import { viewer, librarySkills } from "../test/fixtures";
import { renderApp } from "../test/render";

afterEach(() => vi.unstubAllGlobals());

describe("Skills", () => {
  it("lists the library's skills with their content as text", async () => {
    fakeApi({ "GET /api/me": { body: viewer }, "GET /api/skills": { body: librarySkills } });
    renderApp("/skills");
    expect(await screen.findByRole("heading", { name: "Skills" })).toBeInTheDocument();
    expect(await screen.findByText("security-review")).toBeInTheDocument();
    expect(screen.getByText(/Look for security bugs\./).tagName).toBe("PRE");
  });
});
