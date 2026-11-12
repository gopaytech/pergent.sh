import { render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router";
import { describe, expect, it } from "vitest";

import type { AttentionView } from "../api";
import AttentionRows from "./Attention";

const row = (kind: string, message: string): AttentionView => ({
  instance: "main",
  project: "acme/payments",
  kind,
  message,
  updatedAt: "2026-11-10T10:00:00Z",
});

function show(items: AttentionView[]) {
  return render(
    <MemoryRouter>
      <AttentionRows items={items} />
    </MemoryRouter>,
  );
}

describe("AttentionRows", () => {
  it("shows a repository whose clones keep failing as Failing, with the engine's message", () => {
    const message =
      "Clones of this repository failed 2 times in the last 7 days. Check the clone token's access and the network to the platform, or the Job memory limit if a clone ran out of memory.";
    show([row("clone_failing", message)]);
    expect(screen.getByText(message)).toBeInTheDocument();
    expect(screen.getByText("Failing")).toHaveClass("pill", "bad");
    expect(screen.getByRole("link", { name: "acme/payments" })).toHaveAttribute("href", "/repos/main/acme/payments?tab=setup");
  });

  it("shows an out-of-memory repository the same way", () => {
    show([row("out_of_memory", "Reviews ran out of memory 2 times in the last 7 days. Raise the Job memory limit.")]);
    expect(screen.getByText("Failing")).toHaveClass("pill", "bad");
  });

  it("shows an API client whose callbacks fail as Failing, linked to the client", () => {
    const message =
      "Callbacks to this client failed for 3 reviews in the last 24 hours. Check its callback URL; the results stay readable with GET /v1/reviews/{id}.";
    show([
      { instance: "", project: "", client: "gateway", kind: "callback_failing", message, updatedAt: "2026-11-10T10:00:00Z" },
      { instance: "", project: "", client: "billing", kind: "callback_failing", message, updatedAt: "2026-11-10T10:00:00Z" },
    ]);
    expect(screen.getAllByText(message)).toHaveLength(2);
    expect(screen.getAllByText("Failing")[0]).toHaveClass("pill", "bad");
    expect(screen.getByRole("link", { name: "API client gateway" })).toHaveAttribute("href", "/api-clients/gateway");
    expect(screen.getByRole("link", { name: "API client billing" })).toHaveAttribute("href", "/api-clients/billing");
  });

  it("shows a kind it does not know as Setup", () => {
    show([row("something_new", "text")]);
    expect(screen.getByText("Setup")).toHaveClass("pill", "warn");
  });
});
