import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { fakeApi } from "../test/fakeApi";
import {
  instanceDashboardGitLab,
  instanceGitHub,
  instanceGitHubOneApp,
  instanceGitLab,
  operator,
  signInFromConfig,
  signInJWT,
  signInNone,
  signInOIDC,
} from "../test/fixtures";
import { renderApp } from "../test/render";

afterEach(() => vi.unstubAllGlobals());

const instances = [instanceGitLab, instanceGitHub, instanceDashboardGitLab];

const card = (name: string) => screen.findByRole("region", { name });

// A read-only card's only button copies its webhook URL.
const onlyCopy = (el: HTMLElement) => {
  const buttons = within(el).getAllByRole("button");
  return buttons.length === 1 && buttons[0].getAttribute("aria-label") === "Copy webhook URL";
};

describe("Integrations", () => {
  it("lists every instance, marking the config's and showing which secrets are set", async () => {
    fakeApi({ "GET /api/me": { body: operator }, "GET /api/admin/instances": { body: instances } });
    renderApp("/admin/integrations");
    const main = await card("GitLab · main");
    expect(within(main).getByText("config")).toBeInTheDocument();
    expect(main).toHaveTextContent("https://gitlab.example.com");
    expect(within(within(main).getByRole("group", { name: "Review token" })).getByText("set")).toBeInTheDocument();
    // Signing in with GitLab or GitHub is gone: no OAuth client to show.
    expect(within(main).queryByRole("group", { name: /OAuth/ })).not.toBeInTheDocument();

    const hub = await card("GitHub · hub");
    expect(within(hub).queryByText("config")).not.toBeInTheDocument();
    expect(hub).toHaveTextContent("https://api.github.example.com");
    expect(hub).toHaveTextContent("Approves with the approve App");
    expect(hub).toHaveTextContent("Approve App ID: 12");
    expect(within(hub).getByRole("group", { name: "Review App private key" })).toHaveTextContent("set · changed 2 h ago");
    expect(within(hub).getByRole("group", { name: "Approve App private key" })).toHaveTextContent("not set");
    expect(await card("GitLab · extra")).toBeInTheDocument();
  });

  it("shows each instance's webhook URL and who sets it", async () => {
    fakeApi({
      "GET /api/me": { body: operator },
      "GET /api/admin/instances": { body: [...instances, instanceGitHubOneApp] },
    });
    renderApp("/admin/integrations");
    const main = await card("GitLab · main");
    expect(main).toHaveTextContent("Webhook URL: https://pergent.example.com/webhooks/gitlab/main");
    expect(main).toHaveTextContent("pergent.sh sets this on each project.");
    const hub = await card("GitHub · hub");
    expect(hub).toHaveTextContent("Webhook URL: https://pergent.example.com/webhooks/github/hub");
    expect(hub).toHaveTextContent("Set this on the review App.");
    expect(await card("GitLab · extra")).toHaveTextContent(
      "Webhook URL: https://pergent.example.com/webhooks/gitlab/extra",
    );
    expect(await card("GitHub · solo")).toHaveTextContent(
      "Webhook URL: https://pergent.example.com/webhooks/github/solo",
    );
  });

  it("copies the webhook URL", async () => {
    const user = userEvent.setup();
    fakeApi({ "GET /api/me": { body: operator }, "GET /api/admin/instances": { body: instances } });
    renderApp("/admin/integrations");
    const hub = await card("GitHub · hub");
    await user.click(within(hub).getByRole("button", { name: "Copy webhook URL" }));
    expect(await navigator.clipboard.readText()).toBe("https://pergent.example.com/webhooks/github/hub");
    expect(within(hub).getByRole("button", { name: "Copied" })).toBeInTheDocument();
  });

  it("goes back to Copy a moment after copying", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime });
      fakeApi({ "GET /api/me": { body: operator }, "GET /api/admin/instances": { body: instances } });
      renderApp("/admin/integrations");
      const hub = await card("GitHub · hub");
      await user.click(within(hub).getByRole("button", { name: "Copy webhook URL" }));
      expect(within(hub).getByRole("button", { name: "Copied" })).toBeInTheDocument();
      vi.advanceTimersByTime(2000);
      expect(await within(hub).findByRole("button", { name: "Copy webhook URL" })).toBeInTheDocument();
    } finally {
      vi.useRealTimers();
    }
  });

  it("says the review App approves, and lists no approve secret for it", async () => {
    fakeApi({ "GET /api/me": { body: operator }, "GET /api/admin/instances": { body: [instanceGitHubOneApp] } });
    renderApp("/admin/integrations");
    const solo = await card("GitHub · solo");
    expect(solo).toHaveTextContent("Approves with the review App");
    expect(solo).not.toHaveTextContent("Approve App ID");
    expect(within(solo).getByRole("group", { name: "Review App private key" })).toBeInTheDocument();
    expect(within(solo).queryByRole("group", { name: /Approve/ })).not.toBeInTheDocument();
  });

  it("gives a config instance no way to change it", async () => {
    fakeApi({ "GET /api/me": { body: operator }, "GET /api/admin/instances": { body: instances } });
    renderApp("/admin/integrations");
    const main = await card("GitLab · main");
    expect(onlyCopy(main)).toBe(true);
    expect(within(main).queryByRole("textbox")).not.toBeInTheDocument();
    expect(main).toHaveTextContent("managed by the config file");
    // The others have them.
    const hub = await card("GitHub · hub");
    expect(within(hub).getAllByRole("button", { name: "Replace" }).length).toBeGreaterThan(0);
    expect(within(hub).getByRole("button", { name: "Remove" })).toBeInTheDocument();
  });

  it("replaces a secret from an empty, write-only input", async () => {
    let listed = 0;
    const fake = fakeApi({
      "GET /api/me": { body: operator },
      "GET /api/admin/instances": () => ({ body: listed++ === 0 ? instances : [instanceGitLab, instanceGitHub, instanceDashboardGitLab] }),
      "PUT /api/admin/instances/extra": { body: instanceDashboardGitLab },
    });
    renderApp("/admin/integrations");
    const extra = await card("GitLab · extra");
    const group = within(extra).getByRole("group", { name: "Review token" });
    await userEvent.click(within(group).getByRole("button", { name: "Replace" }));
    const input = within(group).getByLabelText("New Review token");
    expect(input).toHaveAttribute("type", "password");
    expect(input).toHaveAttribute("autocomplete", "new-password");
    expect(input).toHaveAttribute("data-1p-ignore");
    expect(input).toHaveAttribute("data-lpignore", "true");
    expect(input).toHaveValue("");
    await userEvent.type(input, "glpat-new-token");
    await userEvent.click(within(group).getByRole("button", { name: "Save" }));
    const [put] = fake.find("PUT", "/api/admin/instances/extra");
    expect(put.body).toEqual({ secrets: { review_token: "glpat-new-token" } });
    expect(put.headers.get("X-CSRF-Token")).toBe("csrf-123");
    // The input is gone again, and the value with it.
    await vi.waitFor(() => expect(within(group).queryByLabelText("New Review token")).not.toBeInTheDocument());
    expect(document.body.textContent).not.toContain("glpat-new-token");
  });

  it("shows why a replacement is refused, and keeps the input open", async () => {
    fakeApi({
      "GET /api/me": { body: operator },
      "GET /api/admin/instances": { body: instances },
      "PUT /api/admin/instances/extra": { status: 400, body: { error: "GitLab refused the token" } },
    });
    renderApp("/admin/integrations");
    const extra = await card("GitLab · extra");
    const group = within(extra).getByRole("group", { name: "Approve token" });
    await userEvent.click(within(group).getByRole("button", { name: "Replace" }));
    await userEvent.type(within(group).getByLabelText("New Approve token"), "nope");
    await userEvent.click(within(group).getByRole("button", { name: "Save" }));
    expect(await within(extra).findByRole("alert")).toHaveTextContent("GitLab refused the token");
    expect(within(group).getByLabelText("New Approve token")).toBeInTheDocument();
  });

  it("rebuilds a GitHub App key the browser flattened to one line", async () => {
    const fake = fakeApi({
      "GET /api/me": { body: operator },
      "GET /api/admin/instances": { body: instances },
      "PUT /api/admin/instances/hub": { body: instanceGitHub },
    });
    renderApp("/admin/integrations");
    const hub = await card("GitHub · hub");
    const group = within(hub).getByRole("group", { name: "Approve App private key" });
    await userEvent.click(within(group).getByRole("button", { name: "Replace" }));
    const body = "A".repeat(64) + "B".repeat(10);
    await userEvent.type(
      within(group).getByLabelText("New Approve App private key"),
      `-----BEGIN RSA PRIVATE KEY----- ${body} -----END RSA PRIVATE KEY-----`,
    );
    await userEvent.click(within(group).getByRole("button", { name: "Save" }));
    const [put] = fake.find("PUT", "/api/admin/instances/hub");
    expect(put.body).toEqual({
      secrets: {
        approve_app_key: `-----BEGIN RSA PRIVATE KEY-----\n${"A".repeat(64)}\n${"B".repeat(10)}\n-----END RSA PRIVATE KEY-----\n`,
      },
    });
  });

  it("rebuilds a key whose line breaks the browser dropped with no separator", async () => {
    const fake = fakeApi({
      "GET /api/me": { body: operator },
      "GET /api/admin/instances": { body: instances },
      "PUT /api/admin/instances/hub": { body: instanceGitHub },
    });
    renderApp("/admin/integrations");
    const group = within(await card("GitHub · hub")).getByRole("group", { name: "Review App private key" });
    await userEvent.click(within(group).getByRole("button", { name: "Replace" }));
    const body = "Zm9v".repeat(20) + "+/==";
    await userEvent.type(
      within(group).getByLabelText("New Review App private key"),
      `-----BEGIN PRIVATE KEY-----${body}-----END PRIVATE KEY-----`,
    );
    await userEvent.click(within(group).getByRole("button", { name: "Save" }));
    const lines = `${body.slice(0, 64)}\n${body.slice(64)}`;
    expect(fake.find("PUT", "/api/admin/instances/hub")[0].body).toEqual({
      secrets: { review_app_key: `-----BEGIN PRIVATE KEY-----\n${lines}\n-----END PRIVATE KEY-----\n` },
    });
  });

  it("adds a GitLab instance without a webhook secret", async () => {
    let listed = 0;
    const fake = fakeApi({
      "GET /api/me": { body: operator },
      "GET /api/admin/instances": () => ({ body: listed++ === 0 ? [instanceGitLab] : [instanceGitLab, instanceDashboardGitLab] }),
      "POST /api/admin/instances": { status: 201, body: instanceDashboardGitLab },
    });
    renderApp("/admin/integrations");
    await userEvent.click(await screen.findByRole("button", { name: "Add GitLab instance" }));
    const form = screen.getByRole("form", { name: "Add GitLab instance" });
    // Every field starts empty, and no webhook secret is asked.
    for (const box of within(form).getAllByLabelText(/./, { selector: "input" })) expect(box).toHaveValue("");
    expect(within(form).queryByLabelText(/webhook/i)).not.toBeInTheDocument();
    for (const label of ["Review token", "Approve token"]) {
      const input = within(form).getByLabelText(label);
      expect(input).toHaveAttribute("type", "password");
      expect(input).toHaveAttribute("autocomplete", "new-password");
      expect(input).toHaveAttribute("data-1p-ignore");
    }
    // The text fields before a password field get no saved username.
    expect(within(form).queryByLabelText(/OAuth/)).not.toBeInTheDocument();
    for (const label of ["Name", "Address"]) {
      expect(within(form).getByLabelText(label)).toHaveAttribute("data-lpignore", "true");
    }
    await userEvent.type(within(form).getByLabelText("Name"), "extra");
    await userEvent.type(within(form).getByLabelText("Address"), "https://gitlab2.example.com");
    await userEvent.type(within(form).getByLabelText("Review token"), "glpat-review");
    await userEvent.type(within(form).getByLabelText("Approve token"), "glpat-approve");
    await userEvent.click(within(form).getByRole("button", { name: "Add" }));

    const [post] = fake.find("POST", "/api/admin/instances");
    expect(post.headers.get("X-CSRF-Token")).toBe("csrf-123");
    expect(post.body).toEqual({
      name: "extra",
      platform: "gitlab",
      url: "https://gitlab2.example.com",
      secrets: { review_token: "glpat-review", approve_token: "glpat-approve" },
    });
    expect(JSON.stringify(post.body)).not.toContain("webhook_secret");
    expect(await card("GitLab · extra")).toBeInTheDocument();
    expect(screen.queryByRole("form", { name: "Add GitLab instance" })).not.toBeInTheDocument();
  });

  // fillGitHub types what every GitHub instance needs, whoever approves.
  const fillGitHub = async (form: HTMLElement) => {
    await userEvent.type(within(form).getByLabelText("Name"), "hub");
    await userEvent.type(within(form).getByLabelText("API address"), "https://api.github.example.com");
    await userEvent.type(within(form).getByLabelText("Review App ID"), "11");
    await userEvent.type(within(form).getByLabelText("Review App private key"), "key-1");
    await userEvent.type(within(form).getByLabelText("Webhook secret"), "hook");
  };

  const openGitHubForm = async () => {
    const fake = fakeApi({
      "GET /api/me": { body: operator },
      "GET /api/admin/instances": { body: [] },
      "POST /api/admin/instances": { status: 201, body: instanceGitHub },
    });
    renderApp("/admin/integrations");
    await userEvent.click(await screen.findByRole("button", { name: "Add GitHub instance" }));
    return { fake, form: screen.getByRole("form", { name: "Add GitHub instance" }) };
  };

  it("approves with the review App by default: one App, no approve field", async () => {
    const { fake, form } = await openGitHubForm();
    const select = within(form).getByLabelText("Approve with");
    expect(select).toHaveValue("review_app");
    expect(within(select).getAllByRole("option").map((o) => o.textContent)).toEqual([
      "The review App (one App)",
      "A separate approve App",
      "A bot user's token",
    ]);
    for (const label of ["Approve App ID", "Approve App private key", "Approve token"]) {
      expect(within(form).queryByLabelText(label)).not.toBeInTheDocument();
    }
    await fillGitHub(form);
    await userEvent.click(within(form).getByRole("button", { name: "Add" }));
    expect(fake.find("POST", "/api/admin/instances")[0].body).toEqual({
      name: "hub",
      platform: "github",
      apiURL: "https://api.github.example.com",
      reviewAppID: 11,
      approveAppID: 11,
      secrets: { review_app_key: "key-1", webhook_secret: "hook" },
    });
  });

  it("adds a GitHub instance that approves with a separate App", async () => {
    const { fake, form } = await openGitHubForm();
    await userEvent.selectOptions(within(form).getByLabelText("Approve with"), "A separate approve App");
    expect(within(form).queryByLabelText("Approve token")).not.toBeInTheDocument();
    await fillGitHub(form);
    await userEvent.type(within(form).getByLabelText("Approve App ID"), "12");
    await userEvent.type(within(form).getByLabelText("Approve App private key"), "key-2");
    await userEvent.click(within(form).getByRole("button", { name: "Add" }));
    expect(fake.find("POST", "/api/admin/instances")[0].body).toEqual({
      name: "hub",
      platform: "github",
      apiURL: "https://api.github.example.com",
      reviewAppID: 11,
      approveAppID: 12,
      secrets: {
        review_app_key: "key-1",
        approve_app_key: "key-2",
        webhook_secret: "hook",
      },
    });
  });

  it("refuses a separate approve App that is the review App", async () => {
    const { fake, form } = await openGitHubForm();
    await userEvent.selectOptions(within(form).getByLabelText("Approve with"), "A separate approve App");
    await fillGitHub(form);
    await userEvent.type(within(form).getByLabelText("Approve App private key"), "key-2");
    await userEvent.type(within(form).getByLabelText("Approve App ID"), "11");
    await userEvent.click(within(form).getByRole("button", { name: "Add" }));
    expect(await within(form).findByRole("alert")).toHaveTextContent("different App");
    expect(fake.find("POST", "/api/admin/instances")).toHaveLength(0);
  });

  it("asks for a bot user's token when that is chosen", async () => {
    const { fake, form } = await openGitHubForm();
    await userEvent.selectOptions(within(form).getByLabelText("Approve with"), "A bot user's token");
    expect(within(form).queryByLabelText("Approve App ID")).not.toBeInTheDocument();
    expect(within(form).queryByLabelText("Approve App private key")).not.toBeInTheDocument();
    await fillGitHub(form);
    await userEvent.type(within(form).getByLabelText("Approve token"), "ghp-approve");
    await userEvent.click(within(form).getByRole("button", { name: "Add" }));
    const body = fake.find("POST", "/api/admin/instances")[0].body as { approveAppID: number; secrets: Record<string, string> };
    expect(body.approveAppID).toBe(0);
    expect(body.secrets).toEqual({ review_app_key: "key-1", approve_token: "ghp-approve", webhook_secret: "hook" });
  });

  it("sends only the secret of the approver finally chosen", async () => {
    const { fake, form } = await openGitHubForm();
    const select = within(form).getByLabelText("Approve with");
    await userEvent.selectOptions(select, "A bot user's token");
    await userEvent.type(within(form).getByLabelText("Approve token"), "ghp-approve");
    await userEvent.selectOptions(select, "The review App (one App)");
    await fillGitHub(form);
    await userEvent.click(within(form).getByRole("button", { name: "Add" }));
    const body = fake.find("POST", "/api/admin/instances")[0].body as { approveAppID: number; secrets: Record<string, string> };
    expect(body.approveAppID).toBe(11);
    expect(body.secrets).toEqual({ review_app_key: "key-1", webhook_secret: "hook" });
  });

  it("shows the reason the platform refused an instance, and keeps the form", async () => {
    fakeApi({
      "GET /api/me": { body: operator },
      "GET /api/admin/instances": { body: [] },
      "POST /api/admin/instances": { status: 400, body: { error: "the review token can't reach GitLab" } },
    });
    renderApp("/admin/integrations");
    await userEvent.click(await screen.findByRole("button", { name: "Add GitLab instance" }));
    const form = screen.getByRole("form", { name: "Add GitLab instance" });
    await userEvent.type(within(form).getByLabelText("Name"), "extra");
    await userEvent.type(within(form).getByLabelText("Address"), "https://gitlab2.example.com");
    await userEvent.type(within(form).getByLabelText("Review token"), "a");
    await userEvent.type(within(form).getByLabelText("Approve token"), "b");
    await userEvent.click(within(form).getByRole("button", { name: "Add" }));
    expect(await within(form).findByRole("alert")).toHaveTextContent("the review token can't reach GitLab");
    expect(within(form).getByLabelText("Name")).toHaveValue("extra");
  });

  it("removes an instance after a confirmation, and shows it as removing until it is gone", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      const confirm = vi.fn(() => true);
      vi.stubGlobal("confirm", confirm);
      let listed = 0;
      const fake = fakeApi({
        "GET /api/me": { body: operator },
        "GET /api/admin/instances": () => {
          const n = listed++;
          return { body: n === 0 ? instances : n < 3 ? [instanceGitLab, instanceGitHub, { ...instanceDashboardGitLab, removing: true }] : [instanceGitLab, instanceGitHub] };
        },
        "DELETE /api/admin/instances/extra": { status: 202, body: { status: "removing" } },
      });
      renderApp("/admin/integrations");
      const extra = await card("GitLab · extra");
      await userEvent.click(within(extra).getByRole("button", { name: "Remove" }));
      expect(confirm).toHaveBeenCalledWith(expect.stringContaining("extra"));
      expect(fake.find("DELETE", "/api/admin/instances/extra")[0].headers.get("X-CSRF-Token")).toBe("csrf-123");
      await vi.waitFor(() => expect(within(screen.getByRole("region", { name: "GitLab · extra" })).getByText("removing")).toBeInTheDocument());
      expect(onlyCopy(screen.getByRole("region", { name: "GitLab · extra" }))).toBe(true);
      // It is asked about again while something is being removed.
      await vi.advanceTimersByTimeAsync(10_000);
      await vi.waitFor(() => expect(screen.queryByRole("region", { name: "GitLab · extra" })).not.toBeInTheDocument());
    } finally {
      vi.useRealTimers();
    }
  });

  it("removes nothing when the confirmation is declined", async () => {
    vi.stubGlobal("confirm", vi.fn(() => false));
    const fake = fakeApi({ "GET /api/me": { body: operator }, "GET /api/admin/instances": { body: instances } });
    renderApp("/admin/integrations");
    await userEvent.click(within(await card("GitLab · extra")).getByRole("button", { name: "Remove" }));
    expect(fake.calls.filter((c) => c.method === "DELETE")).toHaveLength(0);
  });
});

describe("Integrations: Sign-in", () => {
  const routes = (signIn: unknown) => ({
    "GET /api/me": { body: operator },
    "GET /api/admin/instances": { body: instances },
    "GET /api/admin/sign-in": { body: signIn },
  });
  // The section once its settings are in.
  const section = async () => {
    const box = await screen.findByRole("region", { name: "Sign-in" });
    await vi.waitFor(() => expect(within(box).queryByText("Loading…")).not.toBeInTheDocument());
    return box;
  };

  it("starts with the mode only when nothing is set up, and says what that means", async () => {
    fakeApi(routes(signInNone));
    renderApp("/admin/integrations");
    const box = await section();
    expect(within(box).getByLabelText("Mode")).toHaveValue("");
    expect(within(box).getAllByRole("option").map((o) => o.textContent)).toEqual(["Not set up", "OIDC", "JWT from a proxy"]);
    expect(box).toHaveTextContent("Only local admins can sign in");
    expect(within(box).queryByLabelText("Issuer")).not.toBeInTheDocument();
    expect(within(box).queryByLabelText("Allowed domains")).not.toBeInTheDocument();
    expect(within(box).queryByRole("button", { name: "Test" })).not.toBeInTheDocument();
  });

  it("shows the OIDC fields and not the JWT ones, with the secret write-only", async () => {
    fakeApi(routes(signInOIDC));
    renderApp("/admin/integrations");
    const box = await section();
    expect(within(box).getByLabelText("Mode")).toHaveValue("oidc");
    expect(within(box).getByLabelText("Label")).toHaveValue("Google");
    expect(within(box).getByLabelText("Issuer")).toHaveValue("https://accounts.google.com");
    expect(within(box).getByLabelText("Client ID")).toHaveValue("123-abc.apps.googleusercontent.com");
    const secret = within(box).getByLabelText("Client secret");
    expect(secret).toHaveAttribute("type", "password");
    expect(secret).toHaveValue("");
    expect(box).toHaveTextContent("set · changed 1 h ago");
    expect(within(box).getByLabelText("Allowed domains")).toHaveValue("example.com");
    expect(within(box).getByLabelText("Allowed emails")).toHaveValue("friend@example.org");
    expect(box).toHaveTextContent("https://dash.example.com/auth/oidc/callback");
    for (const jwtOnly of ["Header", "Audience", "JWKS URL", "Email claim"]) {
      expect(within(box).queryByLabelText(jwtOnly)).not.toBeInTheDocument();
    }
  });

  it("shows the JWT fields and not the OIDC ones", async () => {
    fakeApi(routes(signInJWT));
    renderApp("/admin/integrations");
    const box = await section();
    expect(within(box).getByLabelText("Header")).toHaveValue("X-Auth-Token");
    expect(within(box).getByLabelText("Issuer")).toHaveValue("https://proxy.example.com");
    expect(within(box).getByLabelText("Audience")).toHaveValue("pergent");
    expect(within(box).getByLabelText("JWKS URL")).toHaveValue("https://proxy.example.com/keys");
    expect(within(box).getByLabelText("Email claim")).toHaveValue("email");
    for (const oidcOnly of ["Label", "Client ID", "Client secret"]) {
      expect(within(box).queryByLabelText(oidcOnly)).not.toBeInTheDocument();
    }
  });

  it("changes the fields when the mode changes", async () => {
    fakeApi(routes(signInNone));
    renderApp("/admin/integrations");
    const box = await section();
    await userEvent.selectOptions(within(box).getByLabelText("Mode"), "oidc");
    expect(within(box).getByLabelText("Client secret")).toBeInTheDocument();
    expect(within(box).getByText("not set")).toBeInTheDocument();
    expect(within(box).queryByLabelText("Header")).not.toBeInTheDocument();
    await userEvent.selectOptions(within(box).getByLabelText("Mode"), "jwt");
    expect(within(box).getByLabelText("Header")).toBeInTheDocument();
    expect(within(box).queryByLabelText("Client secret")).not.toBeInTheDocument();
  });

  it("saves OIDC with the secret left out when none is typed, and lists one per line", async () => {
    const fake = fakeApi({ ...routes(signInOIDC), "PUT /api/admin/sign-in": { body: signInOIDC } });
    renderApp("/admin/integrations");
    const box = await section();
    await userEvent.clear(within(box).getByLabelText("Allowed domains"));
    await userEvent.type(within(box).getByLabelText("Allowed domains"), "example.com{Enter}Corp.Example{Enter}");
    await userEvent.click(within(box).getByRole("button", { name: "Save" }));
    const [put] = fake.find("PUT", "/api/admin/sign-in");
    expect(put.headers.get("X-CSRF-Token")).toBe("csrf-123");
    expect(put.body).toEqual({
      mode: "oidc",
      label: "Google",
      issuer: "https://accounts.google.com",
      clientID: "123-abc.apps.googleusercontent.com",
      header: "",
      audience: "",
      jwksURL: "",
      emailClaim: "",
      allowedDomains: ["example.com", "Corp.Example"],
      allowedEmails: ["friend@example.org"],
    });
    expect(put.body).not.toHaveProperty("clientSecret");
  });

  it("sends a typed secret once, clears the field and never shows it", async () => {
    const fake = fakeApi({ ...routes(signInNone), "PUT /api/admin/sign-in": { body: signInOIDC } });
    renderApp("/admin/integrations");
    const box = await section();
    await userEvent.selectOptions(within(box).getByLabelText("Mode"), "oidc");
    await userEvent.type(within(box).getByLabelText("Label"), "Google");
    await userEvent.type(within(box).getByLabelText("Issuer"), "https://accounts.google.com");
    await userEvent.type(within(box).getByLabelText("Client ID"), "123-abc.apps.googleusercontent.com");
    await userEvent.type(within(box).getByLabelText("Client secret"), "GOCSPX-very-secret");
    await userEvent.click(within(box).getByRole("button", { name: "Save" }));
    const [put] = fake.find("PUT", "/api/admin/sign-in");
    expect((put.body as { clientSecret?: string }).clientSecret).toBe("GOCSPX-very-secret");
    await vi.waitFor(() => expect(within(box).getByLabelText("Client secret")).toHaveValue(""));
    expect(document.body.textContent).not.toContain("GOCSPX-very-secret");
    expect(box).toHaveTextContent("set");
  });

  it("asks for the secret again when the issuer changes", async () => {
    const fake = fakeApi({ ...routes(signInOIDC), "PUT /api/admin/sign-in": { body: signInOIDC } });
    renderApp("/admin/integrations");
    const box = await section();
    expect(box).not.toHaveTextContent("Enter the client secret again");
    expect(within(box).getByLabelText("Client secret")).not.toBeRequired();
    await userEvent.clear(within(box).getByLabelText("Issuer"));
    await userEvent.type(within(box).getByLabelText("Issuer"), "https://login.example.net");
    expect(box).toHaveTextContent("Enter the client secret again");
    expect(box).toHaveTextContent("never sent to a new issuer");
    expect(within(box).getByLabelText("Client secret")).toBeRequired();
    // Put the issuer back and the hint goes.
    await userEvent.clear(within(box).getByLabelText("Issuer"));
    await userEvent.type(within(box).getByLabelText("Issuer"), "https://accounts.google.com");
    expect(box).not.toHaveTextContent("Enter the client secret again");
    expect(fake.find("PUT", "/api/admin/sign-in")).toHaveLength(0);
  });

  it("saves JWT without the OIDC fields", async () => {
    const fake = fakeApi({ ...routes(signInNone), "PUT /api/admin/sign-in": { body: signInJWT } });
    renderApp("/admin/integrations");
    const box = await section();
    await userEvent.selectOptions(within(box).getByLabelText("Mode"), "jwt");
    await userEvent.type(within(box).getByLabelText("Header"), "X-Auth-Token");
    await userEvent.type(within(box).getByLabelText("Issuer"), "https://proxy.example.com");
    await userEvent.type(within(box).getByLabelText("Audience"), "pergent");
    await userEvent.type(within(box).getByLabelText("JWKS URL"), "https://proxy.example.com/keys");
    await userEvent.type(within(box).getByLabelText("Allowed emails"), "a@example.org");
    await userEvent.click(within(box).getByRole("button", { name: "Save" }));
    expect(fake.find("PUT", "/api/admin/sign-in")[0].body).toEqual({
      mode: "jwt",
      label: "",
      issuer: "https://proxy.example.com",
      clientID: "",
      header: "X-Auth-Token",
      audience: "pergent",
      jwksURL: "https://proxy.example.com/keys",
      emailClaim: "",
      allowedDomains: [],
      allowedEmails: ["a@example.org"],
    });
  });

  it("is read-only when the config file sets it, with the secret only set or not set", async () => {
    const fake = fakeApi(routes(signInFromConfig));
    renderApp("/admin/integrations");
    const box = await section();
    expect(box).toHaveTextContent("config");
    expect(box).toHaveTextContent("managed by the config file");
    expect(box).toHaveTextContent("OIDC");
    expect(box).toHaveTextContent("https://accounts.google.com");
    expect(box).toHaveTextContent("123-abc.apps.googleusercontent.com");
    expect(box).toHaveTextContent("example.com");
    expect(within(box).getByText("set")).toBeInTheDocument();
    expect(within(box).queryByRole("textbox")).not.toBeInTheDocument();
    expect(within(box).queryByLabelText("Client secret")).not.toBeInTheDocument();
    expect(within(box).queryByRole("button", { name: "Save" })).not.toBeInTheDocument();
    expect(fake.calls.filter((c) => c.method === "PUT")).toHaveLength(0);
  });

  it("tests the provider and shows only whether it worked", async () => {
    let worked = true;
    const fake = fakeApi({
      ...routes(signInOIDC),
      "POST /api/admin/sign-in/test": () => ({ body: { ok: worked } }),
    });
    renderApp("/admin/integrations");
    const box = await section();
    await userEvent.click(within(box).getByRole("button", { name: "Test" }));
    expect(await within(box).findByRole("status")).toHaveTextContent("It worked.");
    expect(fake.find("POST", "/api/admin/sign-in/test")[0].body).toEqual({
      mode: "oidc",
      issuer: "https://accounts.google.com",
      jwksURL: "",
    });
    worked = false;
    await userEvent.click(within(box).getByRole("button", { name: "Test" }));
    await vi.waitFor(() => expect(within(box).getByRole("status")).toHaveTextContent("It did not work."));
    expect(within(box).getByRole("status").textContent).toBe("It did not work.");
  });

  it("tests the key set in JWT mode", async () => {
    const fake = fakeApi({ ...routes(signInJWT), "POST /api/admin/sign-in/test": { body: { ok: true } } });
    renderApp("/admin/integrations");
    const box = await section();
    await userEvent.click(within(box).getByRole("button", { name: "Test" }));
    await within(box).findByRole("status");
    expect(fake.find("POST", "/api/admin/sign-in/test")[0].body).toEqual({
      mode: "jwt",
      issuer: "https://proxy.example.com",
      jwksURL: "https://proxy.example.com/keys",
    });
  });

  it("shows the server's text when a save is refused, and keeps what was typed", async () => {
    fakeApi({
      ...routes(signInOIDC),
      "PUT /api/admin/sign-in": { status: 400, body: { error: "issuer must be an https URL" } },
    });
    renderApp("/admin/integrations");
    const box = await section();
    await userEvent.clear(within(box).getByLabelText("Label"));
    await userEvent.type(within(box).getByLabelText("Label"), "Workspace");
    await userEvent.click(within(box).getByRole("button", { name: "Save" }));
    expect(await within(box).findByRole("alert")).toHaveTextContent("issuer must be an https URL");
    expect(within(box).getByLabelText("Label")).toHaveValue("Workspace");
  });
});
