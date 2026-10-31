import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render } from "@testing-library/react";
import { MemoryRouter, useLocation } from "react-router";

import App from "../App";

// Location shows the router's current path, for tests that check where a
// page sent the user.
function Location() {
  const loc = useLocation();
  return <output data-testid="location">{loc.pathname + loc.search}</output>;
}

// renderApp renders the whole app at path, with a fresh query cache and no
// retries; it returns the cache's client with the render result.
export function renderApp(path: string) {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Infinity }, mutations: { retry: false } },
  });
  const result = render(
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={[path]}>
        <App />
        <Location />
      </MemoryRouter>
    </QueryClientProvider>,
  );
  return { ...result, client };
}
