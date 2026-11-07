import { screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { fakeApi } from "../test/fakeApi";
import { operator, overview, viewer } from "../test/fixtures";
import { renderApp } from "../test/render";

afterEach(() => vi.unstubAllGlobals());

const options = (extra: object = {}) => ({ body: { password: true, jwt: false, ...extra } });

describe("SignIn", () => {
  it("has the SSO button, to the OIDC login route, and nothing else", async () => {
    const fake = fakeApi({ "GET /api/sign-in-options": options({ sso: { label: "Google" } }) });
    renderApp("/sign-in");
    expect(await screen.findByRole("heading", { name: "Sign in to pergent.sh" })).toBeInTheDocument();
    expect(await screen.findByRole("link", { name: "Sign in with Google" })).toHaveAttribute("href", "/auth/oidc/login");
    // No password form, even though the server has local admins.
    expect(screen.queryByLabelText("Username")).not.toBeInTheDocument();
    expect(screen.queryByLabelText("Password")).not.toBeInTheDocument();
    expect(screen.queryByText("Sign-in isn't set up yet. Ask your admin.")).not.toBeInTheDocument();
    // The only link is the button: nothing leads to the admin sign-in.
    expect(screen.getAllByRole("link")).toHaveLength(1);
    expect(document.querySelector("[href*='admin']")).toBeNull();
    expect(document.body).not.toHaveTextContent(/admin\/sign-in/);
    expect(fake.find("POST", "/auth/password")).toHaveLength(0);
    expect(fake.find("GET", "/auth/csrf")).toHaveLength(0);
  });

  it("says sign-in isn't set up when there is no SSO", async () => {
    fakeApi({ "GET /api/sign-in-options": options() });
    renderApp("/sign-in");
    expect(await screen.findByText("Sign-in isn't set up yet. Ask your admin.")).toBeInTheDocument();
    expect(screen.queryByRole("link")).not.toBeInTheDocument();
    expect(screen.queryByLabelText("Username")).not.toBeInTheDocument();
  });

  it("has no SSO button when sso is null", async () => {
    fakeApi({ "GET /api/sign-in-options": options({ sso: null }) });
    renderApp("/sign-in");
    expect(await screen.findByText("Sign-in isn't set up yet. Ask your admin.")).toBeInTheDocument();
  });

  it("shows the error and not the 'not set up' text when the options can't be read", async () => {
    fakeApi({ "GET /api/sign-in-options": { status: 500, body: { error: "try again" } } });
    renderApp("/sign-in");
    expect(await screen.findByRole("alert")).toHaveTextContent("try again");
    expect(screen.queryByText("Sign-in isn't set up yet. Ask your admin.")).not.toBeInTheDocument();
  });

  it("tells a refused person to ask an admin", async () => {
    fakeApi({ "GET /api/sign-in-options": options({ sso: { label: "Google" } }) });
    renderApp("/sign-in?error=not_allowed");
    expect(await screen.findByRole("alert")).toHaveTextContent("Ask an admin to add you");
    expect(await screen.findByRole("link", { name: "Sign in with Google" })).toBeInTheDocument();
  });

  it("shows a message only for the codes it knows", async () => {
    fakeApi({ "GET /api/sign-in-options": options({ sso: { label: "Google" } }) });
    renderApp("/sign-in?error=%3Cb%3Ehello%3C%2Fb%3E");
    await screen.findByRole("link", { name: "Sign in with Google" });
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
    const card = screen.getByRole("heading", { name: "Sign in to pergent.sh" }).parentElement;
    expect(card).not.toHaveTextContent(/hello/);
  });

  it("explains each refusal code", async () => {
    for (const [code, text] of [
      ["not_allowed", /Ask an admin to add you/],
      ["expired", /expired/],
      ["failed", /Sign-in failed/],
      ["denied", /not completed/],
      ["not_set_up", /isn't set up yet/],
      ["unavailable", /can't be reached/],
    ] as const) {
      fakeApi({ "GET /api/sign-in-options": options({ sso: { label: "Google" } }) });
      const { unmount } = renderApp(`/sign-in?error=${code}`);
      expect(await screen.findByRole("alert")).toHaveTextContent(text);
      unmount();
    }
  });

  it("keeps the refusal code when it comes to the old address", async () => {
    fakeApi({ "GET /api/sign-in-options": options({ sso: { label: "Google" } }) });
    renderApp("/signin?error=not_allowed");
    expect(await screen.findByRole("alert")).toHaveTextContent("Ask an admin to add you");
    expect(screen.getByTestId("location")).toHaveTextContent(/^\/sign-in\?error=not_allowed$/);
  });

  it("sends a browser that is signed in already on, to the user pages", async () => {
    fakeApi({
      "GET /api/sign-in-options": options({ sso: { label: "Google" } }),
      "GET /api/me": { body: viewer },
      "GET /api/overview?days=7": { body: overview },
    });
    renderApp("/sign-in");
    expect(await screen.findByRole("navigation", { name: "Main" })).toBeInTheDocument();
    expect(screen.getByTestId("location")).toHaveTextContent(/^\/$/);
  });

  it("does not take an expected 401 for a signed-out session", async () => {
    // A 401 from the probe must not clear the cache and navigate (a loop).
    const fake = fakeApi({
      "GET /api/sign-in-options": options({ sso: { label: "Google" } }),
      "GET /api/me": { status: 401, body: { error: "not signed in" } },
    });
    renderApp("/sign-in");
    await screen.findByRole("link", { name: "Sign in with Google" });
    expect(screen.getByTestId("location")).toHaveTextContent(/^\/sign-in$/);
    expect(fake.find("GET", "/api/me")).toHaveLength(1);
  });

  describe("with JWT sign-in", () => {
    it("has no button to press, and says the page load signs the person in", async () => {
      fakeApi({
        "GET /api/sign-in-options": options({ jwt: true }),
        "GET /api/me": { status: 401, body: { error: "not signed in" } },
      });
      renderApp("/sign-in");
      expect(await screen.findByText(/signs you in before this page loads/)).toBeInTheDocument();
      expect(screen.queryByRole("link")).not.toBeInTheDocument();
      expect(screen.queryByText("Sign-in isn't set up yet. Ask your admin.")).not.toBeInTheDocument();
    });

    it("sends the person on once the page load has signed them in", async () => {
      fakeApi({
        "GET /api/sign-in-options": options({ jwt: true }),
        "GET /api/me": { body: operator },
        "GET /api/overview?days=7": { body: overview },
      });
      renderApp("/sign-in");
      expect(await screen.findByRole("navigation", { name: "Main" })).toBeInTheDocument();
      expect(screen.getByTestId("location")).toHaveTextContent(/^\/$/);
    });

    it("tells a person the proxy vouches for, but nobody added, to ask an admin", async () => {
      fakeApi({
        "GET /api/sign-in-options": options({ jwt: true }),
        "GET /api/me": { status: 401, body: { error: "ask an admin to add you", code: "not_allowed" } },
      });
      renderApp("/sign-in");
      expect(await screen.findByRole("alert")).toHaveTextContent("Ask an admin to add you");
    });

    it("shows an outage of the signing keys", async () => {
      fakeApi({
        "GET /api/sign-in-options": options({ jwt: true }),
        "GET /api/me": { status: 503, body: { error: "sign-in is unavailable: the signing keys could not be read; try again in a moment" } },
      });
      renderApp("/sign-in");
      expect(await screen.findByRole("alert")).toHaveTextContent(/sign-in is unavailable/);
    });
  });
});
