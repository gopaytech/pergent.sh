import { useQueryClient } from "@tanstack/react-query";
import { useCallback } from "react";

import { api, isCSRFError, type Me, type WriteMethod } from "../api";
import { useMe } from "./me";

// useCSRFRetry returns the function writes are made with: it calls send
// with the session's CSRF token; when the server says the token is stale,
// it reads /api/me again and calls send once more with the new one.
export function useCSRFRetry() {
  const me = useMe();
  const client = useQueryClient();
  return useCallback(
    async <T>(send: (csrf: string) => Promise<T>): Promise<T> => {
      try {
        return await send(me.csrfToken);
      } catch (err) {
        if (!isCSRFError(err)) throw err;
        const fresh = await client.fetchQuery({
          queryKey: ["me"],
          queryFn: () => api.get<Me>("/api/me"),
          staleTime: 0,
        });
        return send(fresh.csrfToken);
      }
    },
    [me.csrfToken, client],
  );
}

// useWrite returns the function pages make writes with, with one CSRF retry.
export function useWrite() {
  const retry = useCSRFRetry();
  return useCallback(
    <T>(method: WriteMethod, path: string, body?: unknown): Promise<T> =>
      retry((csrf) => api.write<T>(method, path, body, csrf)),
    [retry],
  );
}
