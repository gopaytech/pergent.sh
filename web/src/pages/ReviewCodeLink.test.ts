import { describe, expect, it } from "vitest";

import { codeLink } from "./ReviewDetail";

describe("codeLink", () => {
  const sha = "a1b2c3d";
  it("links a line on GitLab, from the merge request's address", () => {
    expect(codeLink("https://gitlab.example.com/group/sub/app/-/merge_requests/77", sha, "internal/x.go", 88)).toBe(
      "https://gitlab.example.com/group/sub/app/-/blob/a1b2c3d/internal/x.go#L88",
    );
  });

  it("links a line on GitHub, from the pull request's address", () => {
    expect(codeLink("https://github.com/acme/app/pull/12/", sha, "cmd/main.go", 3)).toBe("https://github.com/acme/app/blob/a1b2c3d/cmd/main.go#L3");
  });

  it("links the file alone with no line, and escapes each part of the path", () => {
    expect(codeLink("https://github.com/acme/app/pull/12", sha, "docs/a b#c.md", null)).toBe(
      "https://github.com/acme/app/blob/a1b2c3d/docs/a%20b%23c.md",
    );
  });

  it("gives no link for an address it doesn't know, or with no commit or file", () => {
    expect(codeLink("https://example.com/review/12", sha, "a.go", 1)).toBeNull();
    expect(codeLink("", sha, "a.go", 1)).toBeNull();
    expect(codeLink("https://github.com/acme/app/pull/12", "", "a.go", 1)).toBeNull();
    expect(codeLink("https://github.com/acme/app/pull/12", sha, "", 1)).toBeNull();
  });
});
