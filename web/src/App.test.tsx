import { render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router";
import { describe, expect, it } from "vitest";

import App from "./App";

function renderAt(path: string) {
  return render(
    <MemoryRouter initialEntries={[path]}>
      <App />
    </MemoryRouter>,
  );
}

describe("App shell", () => {
  it("shows the menu and the overview placeholder", () => {
    renderAt("/");
    const nav = screen.getByRole("navigation", { name: "Main" });
    for (const label of ["Overview", "Repositories", "Reviews", "Skills", "Integrations", "Settings", "Organization"]) {
      expect(nav).toHaveTextContent(label);
    }
    expect(screen.getByRole("heading", { name: "Overview" })).toBeInTheDocument();
  });

  it("marks the current page in the menu", () => {
    renderAt("/repos");
    expect(screen.getByRole("link", { name: "Repositories" })).toHaveClass("active");
    expect(screen.getByRole("link", { name: "Overview" })).not.toHaveClass("active");
  });
});
