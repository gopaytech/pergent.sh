import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { fakeApi } from "../test/fakeApi";
import {
  instanceDashboardGitLab,
  instanceGitHub,
  instanceGitHubOneApp,
  instanceGitLab,
  integrations,
  integrationsGitHub,
  operator,
  platformAdmin,
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

describe("Integrations for a platform admin", () => {
  it("shows the instance, the approver and each repository's installation, read-only", async () => {
    const fake = fakeApi({ "GET /api/me": { body: platformAdmin }, "GET /api/integrations": { body: integrations } });
    renderApp("/integrations");
    expect(await screen.findByRole("heading", { name: "Integrations" })).toBeInTheDocument();
    const inst = await screen.findByRole("region", { name: "GitLab · main" });
    expect(within(inst).getByText("Connected")).toHaveClass("pill", "ok");
    expect(inst).toHaveTextContent("Approves with a bot user's token");
    expect(inst).toHaveTextContent("Token expires 2026-11-09");
    const row = screen.getByText("acme/payments").closest("tr") as HTMLElement;
    expect(within(row).getByText("Installed")).toHaveClass("pill", "ok");
    expect(within(row).getByText("Can't approve")).toHaveClass("pill", "bad");
    expect(row).toHaveTextContent("Inline findings are off: the review App has Pull requests: Read-only.");
    const widgets = screen.getByText("acme/widgets").closest("tr") as HTMLElement;
    expect(widgets).not.toHaveTextContent("Inline findings");
    expect(screen.queryByRole("button", { name: /Add .* instance/ })).not.toBeInTheDocument();
    expect(fake.find("GET", "/api/admin/instances")).toHaveLength(0);
  });
});

describe("Integrations on GitHub", () => {
  const note =
    "Can't read classic branch protection (the review App lacks Administration: Read); a \"require conversation resolution\" rule there is treated as off.";

  it("says when classic branch protection can't be read, only where inline findings are on", async () => {
    fakeApi({ "GET /api/me": { body: platformAdmin }, "GET /api/integrations": { body: integrationsGitHub } });
    renderApp("/integrations");
    const orders = (await screen.findByText("acme/orders")).closest("tr") as HTMLElement;
    expect(orders).toHaveTextContent(note);
    expect(orders).not.toHaveTextContent("Inline findings");
    const payments = screen.getByText("acme/payments").closest("tr") as HTMLElement;
    expect(payments).toHaveTextContent("Inline findings are off");
    expect(payments).not.toHaveTextContent("classic branch protection");
    const widgets = screen.getByText("acme/widgets").closest("tr") as HTMLElement;
    expect(widgets).not.toHaveTextContent("classic branch protection");
  });

  it("does not raise it as a Needs attention item", async () => {
    fakeApi({ "GET /api/me": { body: platformAdmin }, "GET /api/integrations": { body: integrationsGitHub } });
    renderApp("/integrations");
    await screen.findByText("acme/orders");
    expect(screen.queryByText(/Needs attention/)).not.toBeInTheDocument();
  });
});

describe("Integrations for an operator", () => {
  it("lists every instance, marking the config's and showing which secrets are set", async () => {
    const fake = fakeApi({ "GET /api/me": { body: operator }, "GET /api/admin/instances": { body: instances } });
    renderApp("/integrations");
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
    expect(fake.find("GET", "/api/integrations")).toHaveLength(0);
  });

  it("shows each instance's webhook URL and who sets it", async () => {
    fakeApi({
      "GET /api/me": { body: operator },
      "GET /api/admin/instances": { body: [...instances, instanceGitHubOneApp] },
    });
    renderApp("/integrations");
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
    renderApp("/integrations");
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
      renderApp("/integrations");
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
    renderApp("/integrations");
    const solo = await card("GitHub · solo");
    expect(solo).toHaveTextContent("Approves with the review App");
    expect(solo).not.toHaveTextContent("Approve App ID");
    expect(within(solo).getByRole("group", { name: "Review App private key" })).toBeInTheDocument();
    expect(within(solo).queryByRole("group", { name: /Approve/ })).not.toBeInTheDocument();
  });

  it("gives a config instance no way to change it", async () => {
    fakeApi({ "GET /api/me": { body: operator }, "GET /api/admin/instances": { body: instances } });
    renderApp("/integrations");
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
    renderApp("/integrations");
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
    renderApp("/integrations");
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
    renderApp("/integrations");
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
    renderApp("/integrations");
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
    renderApp("/integrations");
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
    renderApp("/integrations");
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
    renderApp("/integrations");
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
      renderApp("/integrations");
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
    renderApp("/integrations");
    await userEvent.click(within(await card("GitLab · extra")).getByRole("button", { name: "Remove" }));
    expect(fake.calls.filter((c) => c.method === "DELETE")).toHaveLength(0);
  });
});
