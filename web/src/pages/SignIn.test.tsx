import { screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { fakeApi } from "../test/fakeApi";
import { renderApp } from "../test/render";

afterEach(() => vi.unstubAllGlobals());

describe("SignIn", () => {
  it("has one button per instance, to its login route", async () => {
    const fake = fakeApi({
      "GET /api/instances": {
        body: [
          { platform: "gitlab", instance: "main", label: "GitLab (main)" },
          { platform: "github", instance: "gh com", label: "GitHub (gh com)" },
          // Not a platform: no button.
          { platform: "javascript:", instance: "x", label: "Evil" },
        ],
      },
    });
    renderApp("/signin");
    expect(await screen.findByRole("heading", { name: "Sign in to pergent.sh" })).toBeInTheDocument();
    expect(await screen.findByRole("link", { name: "Sign in with GitLab (main)" })).toHaveAttribute(
      "href",
      "/auth/gitlab/main/login",
    );
    expect(screen.getByRole("link", { name: "Sign in with GitHub (gh com)" })).toHaveAttribute(
      "href",
      "/auth/github/gh%20com/login",
    );
    expect(screen.queryByRole("link", { name: /Evil/ })).not.toBeInTheDocument();
    // The sign-in page asks nothing of the session.
    expect(fake.find("GET", "/api/me")).toHaveLength(0);
  });

  it("says when no instance has sign-in", async () => {
    fakeApi({ "GET /api/instances": { body: [] } });
    renderApp("/signin");
    expect(await screen.findByText(/No sign-in is configured/)).toBeInTheDocument();
  });
});
