import { useQueryClient } from "@tanstack/react-query";
import { useCallback } from "react";

import { api, isCSRFError, type Me, type WriteMethod } from "../api";
import { useMe } from "./me";

// useWrite returns the function pages make writes with. It sends the
// session's CSRF token; when the server says the token is stale, it reads
// /api/me again and tries the write once more with the new one.
export function useWrite() {
  const me = useMe();
  const client = useQueryClient();
  return useCallback(
    async <T>(method: WriteMethod, path: string, body?: unknown): Promise<T> => {
      try {
        return await api.write<T>(method, path, body, me.csrfToken);
      } catch (err) {
        if (!isCSRFError(err)) throw err;
        const fresh = await client.fetchQuery({
          queryKey: ["me"],
          queryFn: () => api.get<Me>("/api/me"),
          staleTime: 0,
        });
        return api.write<T>(method, path, body, fresh.csrfToken);
      }
    },
    [me.csrfToken, client],
  );
}
