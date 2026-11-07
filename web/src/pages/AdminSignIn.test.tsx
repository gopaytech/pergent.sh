import { screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { fakeApi } from "../test/fakeApi";
import { operator, overview, viewer } from "../test/fixtures";
import { renderApp } from "../test/render";

afterEach(() => vi.unstubAllGlobals());

const options = (extra: object = {}) => ({ body: { password: true, jwt: false, ...extra } });

async function fill(username: string, password: string) {
  await userEvent.type(await screen.findByLabelText("Username"), username);
  await userEvent.type(screen.getByLabelText("Password"), password);
  await userEvent.click(screen.getByRole("button", { name: "Sign in" }));
}

describe("AdminSignIn", () => {
  it("has the password form, and no SSO button when SSO is not set up", async () => {
    fakeApi({ "GET /api/sign-in-options": options() });
    renderApp("/admin/sign-in");
    expect(await screen.findByRole("heading", { name: "Admin sign-in" })).toBeInTheDocument();
    expect(await screen.findByLabelText("Username")).toBeInTheDocument();
    expect(screen.getByLabelText("Password")).toBeInTheDocument();
    expect(screen.queryByRole("link")).not.toBeInTheDocument();
  });

  it("has the SSO button as well when SSO is set up, below the form", async () => {
    fakeApi({ "GET /api/sign-in-options": options({ sso: { label: "Google" } }) });
    renderApp("/admin/sign-in");
    const form = await screen.findByRole("button", { name: "Sign in" });
    const button = await screen.findByRole("link", { name: "Sign in with Google" });
    expect(button).toHaveAttribute("href", "/auth/oidc/login");
    expect(form.compareDocumentPosition(button) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it("posts the username and password with the CSRF token from /auth/csrf, then opens the admin area", async () => {
    let signedIn = false;
    const fake = fakeApi({
      "GET /api/sign-in-options": options(),
      "GET /auth/csrf": { body: { csrfToken: "login-csrf" } },
      "POST /auth/password": () => {
        signedIn = true;
        return { status: 204 };
      },
      "GET /api/me?probe=1": () => (signedIn ? { body: operator } : { status: 401, body: { error: "not signed in" } }),
      "GET /api/me": { body: operator },
    });
    renderApp("/admin/sign-in");
    await fill("admin", "correct horse battery");
    expect(await screen.findByRole("heading", { name: "Admin" })).toBeInTheDocument();
    const [post] = fake.find("POST", "/auth/password");
    expect(post.body).toEqual({ username: "admin", password: "correct horse battery" });
    expect(post.headers.get("X-CSRF-Token")).toBe("login-csrf");
    expect(fake.find("GET", "/auth/csrf")).toHaveLength(1);
    expect(screen.getByTestId("location")).toHaveTextContent(/^\/admin$/);
  });

  it("empties the password, not the username, after a failed sign-in", async () => {
    fakeApi({
      "GET /api/sign-in-options": options(),
      "GET /auth/csrf": { body: { csrfToken: "login-csrf" } },
      "POST /auth/password": { status: 401, body: { error: "wrong username or password" } },
    });
    renderApp("/admin/sign-in");
    await fill("admin", "nope nope nope");
    expect(await screen.findByRole("alert")).toHaveTextContent("wrong username or password");
    expect(screen.getByLabelText("Password")).toHaveValue("");
    expect(screen.getByLabelText("Username")).toHaveValue("admin");
    expect(screen.getByTestId("location")).toHaveTextContent(/^\/admin\/sign-in$/);
  });

  it("keeps no closure with the password once the form is gone", async () => {
    fakeApi({
      "GET /api/sign-in-options": options(),
      "GET /auth/csrf": { body: { csrfToken: "login-csrf" } },
      "POST /auth/password": { status: 401, body: { error: "wrong username or password" } },
    });
    const { client, unmount } = renderApp("/admin/sign-in");
    await fill("admin", "nope nope nope");
    await screen.findByRole("alert");
    unmount();
    await vi.waitFor(() => expect(client.getMutationCache().getAll()).toHaveLength(0));
  });

  it("shows the text of a rate limit", async () => {
    fakeApi({
      "GET /api/sign-in-options": options(),
      "GET /auth/csrf": { body: { csrfToken: "login-csrf" } },
      "POST /auth/password": { status: 429, body: { error: "too many sign-in attempts, try again later" } },
    });
    renderApp("/admin/sign-in");
    await fill("admin", "whatever whatever");
    expect(await screen.findByRole("alert")).toHaveTextContent("too many sign-in attempts, try again later");
  });

  it("says so when the CSRF token can't be fetched", async () => {
    const fake = fakeApi({
      "GET /api/sign-in-options": options(),
      "GET /auth/csrf": { status: 500, body: { error: "try again" } },
    });
    renderApp("/admin/sign-in");
    await fill("admin", "whatever whatever");
    expect(await screen.findByRole("alert")).toHaveTextContent("try again");
    expect(fake.find("POST", "/auth/password")).toHaveLength(0);
  });

  it("lands a signed-in admin on /admin and a signed-in user on /", async () => {
    fakeApi({ "GET /api/sign-in-options": options(), "GET /api/me?probe=1": { body: operator }, "GET /api/me": { body: operator } });
    const first = renderApp("/admin/sign-in");
    expect(await screen.findByRole("heading", { name: "Admin" })).toBeInTheDocument();
    expect(screen.getByTestId("location")).toHaveTextContent(/^\/admin$/);
    first.unmount();

    fakeApi({ "GET /api/sign-in-options": options(), "GET /api/me?probe=1": { body: viewer }, "GET /api/me": { body: viewer }, "GET /api/overview?days=7": { body: overview } });
    renderApp("/admin/sign-in");
    expect(await screen.findByRole("heading", { name: "Overview" })).toBeInTheDocument();
    expect(screen.getByTestId("location")).toHaveTextContent(/^\/$/);
  });

  it("reads /api/me quietly (?probe=1), so a proxy's header can't sign the person in before the password form", async () => {
    const fake = fakeApi({
      "GET /api/sign-in-options": options(),
      "GET /api/me?probe=1": { status: 401, body: { error: "not signed in" } },
    });
    renderApp("/admin/sign-in");
    expect(await screen.findByLabelText("Username")).toBeInTheDocument();
    expect(fake.find("GET", "/api/me?probe=1")).toHaveLength(1);
    expect(fake.find("GET", "/api/me")).toHaveLength(0);
  });
});
