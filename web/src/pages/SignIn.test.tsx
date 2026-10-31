import { screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { fakeApi } from "../test/fakeApi";
import { operator, overview } from "../test/fixtures";
import { renderApp } from "../test/render";

afterEach(() => vi.unstubAllGlobals());

const instances = {
  password: true,
  instances: [
    { platform: "gitlab", instance: "main", label: "GitLab (main)" },
    { platform: "github", instance: "gh com", label: "GitHub (gh com)" },
    // Not a platform: no button.
    { platform: "javascript:", instance: "x", label: "Evil" },
  ],
};

async function fill(username: string, password: string) {
  await userEvent.type(await screen.findByLabelText("Username"), username);
  await userEvent.type(screen.getByLabelText("Password"), password);
  await userEvent.click(screen.getByRole("button", { name: "Sign in" }));
}

describe("SignIn", () => {
  it("has one button per instance, to its login route", async () => {
    const fake = fakeApi({ "GET /api/instances": { body: instances } });
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

  it("shows only the password form when no instance has sign-in", async () => {
    fakeApi({ "GET /api/instances": { body: { password: true, instances: [] } } });
    renderApp("/signin");
    expect(await screen.findByLabelText("Username")).toBeInTheDocument();
    expect(screen.queryByRole("link")).not.toBeInTheDocument();
  });

  it("empties the password, not the username, after a failed sign-in", async () => {
    fakeApi({
      "GET /api/instances": { body: instances },
      "GET /auth/csrf": { body: { csrfToken: "login-csrf" } },
      "POST /auth/password": { status: 401, body: { error: "wrong username or password" } },
    });
    renderApp("/signin");
    await fill("admin", "nope nope nope");
    expect(await screen.findByRole("alert")).toBeInTheDocument();
    expect(screen.getByLabelText("Password")).toHaveValue("");
    expect(screen.getByLabelText("Username")).toHaveValue("admin");
  });

  it("keeps no closure with the password once the form is gone", async () => {
    fakeApi({
      "GET /api/instances": { body: instances },
      "GET /auth/csrf": { body: { csrfToken: "login-csrf" } },
      "POST /auth/password": { status: 401, body: { error: "wrong username or password" } },
    });
    const { client, unmount } = renderApp("/signin");
    await fill("admin", "nope nope nope");
    await screen.findByRole("alert");
    unmount();
    await vi.waitFor(() => expect(client.getMutationCache().getAll()).toHaveLength(0));
  });

  it("says no sign-in is configured when there is no password form and no button", async () => {
    fakeApi({ "GET /api/instances": { body: { password: false, instances: [{ platform: "javascript:", instance: "x", label: "Evil" }] } } });
    renderApp("/signin");
    expect(await screen.findByText("No sign-in is configured.")).toBeInTheDocument();
    expect(screen.queryByLabelText("Username")).not.toBeInTheDocument();
  });

  it("says no sign-in is configured when the instances can't be read", async () => {
    fakeApi({ "GET /api/instances": { status: 500, body: { error: "try again" } } });
    renderApp("/signin");
    expect(await screen.findByRole("alert")).toHaveTextContent("try again");
    expect(screen.getByText("No sign-in is configured.")).toBeInTheDocument();
  });

  it("doesn't say it while a button or the password form is there", async () => {
    fakeApi({ "GET /api/instances": { body: { password: false, instances: [instances.instances[0]] } } });
    renderApp("/signin");
    await screen.findByRole("link", { name: "Sign in with GitLab (main)" });
    expect(screen.queryByText("No sign-in is configured.")).not.toBeInTheDocument();
  });

  it("shows the password form above the platform buttons", async () => {
    fakeApi({ "GET /api/instances": { body: instances } });
    renderApp("/signin");
    const form = await screen.findByRole("button", { name: "Sign in" });
    const button = await screen.findByRole("link", { name: "Sign in with GitLab (main)" });
    expect(form.compareDocumentPosition(button) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it("posts the username and password with the CSRF token from /auth/csrf, then opens the dashboard", async () => {
    const fake = fakeApi({
      "GET /api/instances": { body: instances },
      "GET /auth/csrf": { body: { csrfToken: "login-csrf" } },
      "POST /auth/password": { status: 204 },
      "GET /api/me": { body: operator },
      "GET /api/overview?days=7": { body: overview },
    });
    renderApp("/signin");
    await fill("admin", "correct horse battery");
    expect(await screen.findByRole("navigation", { name: "Main" })).toBeInTheDocument();
    const [post] = fake.find("POST", "/auth/password");
    expect(post.body).toEqual({ username: "admin", password: "correct horse battery" });
    expect(post.headers.get("X-CSRF-Token")).toBe("login-csrf");
    expect(fake.find("GET", "/auth/csrf")).toHaveLength(1);
    expect(screen.getByTestId("location")).toHaveTextContent(/^\/$/);
  });

  it("shows the server's text for a wrong password and stays on the form", async () => {
    fakeApi({
      "GET /api/instances": { body: instances },
      "GET /auth/csrf": { body: { csrfToken: "login-csrf" } },
      "POST /auth/password": { status: 401, body: { error: "wrong username or password" } },
    });
    renderApp("/signin");
    await fill("admin", "nope nope nope");
    expect(await screen.findByRole("alert")).toHaveTextContent("wrong username or password");
    expect(screen.getByTestId("location")).toHaveTextContent(/^\/signin$/);
    expect(screen.getByLabelText("Username")).toHaveValue("admin");
  });

  it("shows the text of a rate limit", async () => {
    fakeApi({
      "GET /api/instances": { body: instances },
      "GET /auth/csrf": { body: { csrfToken: "login-csrf" } },
      "POST /auth/password": { status: 429, body: { error: "too many sign-in attempts, try again later" } },
    });
    renderApp("/signin");
    await fill("admin", "whatever whatever");
    expect(await screen.findByRole("alert")).toHaveTextContent("too many sign-in attempts, try again later");
  });

  it("says so when the CSRF token can't be fetched", async () => {
    const fake = fakeApi({
      "GET /api/instances": { body: instances },
      "GET /auth/csrf": { status: 500, body: { error: "try again" } },
    });
    renderApp("/signin");
    await fill("admin", "whatever whatever");
    expect(await screen.findByRole("alert")).toHaveTextContent("try again");
    expect(fake.find("POST", "/auth/password")).toHaveLength(0);
  });
});
