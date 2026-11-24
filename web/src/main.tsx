import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { RouterProvider, createBrowserRouter } from "react-router";

import { ApiError } from "./api";
import App from "./App";
import "./theme.css";
import "./styles.css";

const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      staleTime: 30_000,
      // An answer like 401, 403 or 404 won't change on a retry.
      retry: (failures, err) => failures < 1 && !(err instanceof ApiError && err.status < 500),
    },
  },
});

const root = document.getElementById("root");
if (!root) throw new Error("#root is missing from index.html");

// A data router, with App's own routes under one catch-all: useBlocker, which
// asks before a page with unsaved changes is left, needs one.
const router = createBrowserRouter([{ path: "*", element: <App /> }]);

createRoot(root).render(
  <StrictMode>
    <QueryClientProvider client={queryClient}>
      <RouterProvider router={router} />
    </QueryClientProvider>
  </StrictMode>,
);
