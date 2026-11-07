import { createContext, useContext } from "react";

import type { Me } from "../api";

// MeContext holds the signed-in user, from GET /api/me. The CSRF token
// lives here, in memory only.
export const MeContext = createContext<Me | null>(null);

// inAdminArea says whether a path is in the admin area (/admin and below).
export const inAdminArea = (pathname: string) => pathname === "/admin" || pathname.startsWith("/admin/");

// useMe returns the signed-in user; only pages inside Layout call it.
export function useMe(): Me {
  const me = useContext(MeContext);
  if (!me) throw new Error("useMe outside Layout");
  return me;
}
