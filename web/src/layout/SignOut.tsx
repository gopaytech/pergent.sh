import { useMutation, useQueryClient } from "@tanstack/react-query";
import { useLocation, useNavigate } from "react-router";

import { ErrorBox } from "../ui";
import { inAdminArea, useMe } from "./me";
import { useWrite } from "./write";

// SignOut ends the session. Only once the server says it has ended does
// the page drop what it loaded and move on; a failure is shown and the user
// stays signed in, and knows it. Where it moves to: /signed-out for a session
// a proxy started (JWT sign-in: the proxy still vouches for the person, so the
// sign-in page would sign them straight in again), else the sign-in page of
// the area the user was in (a local admin always to /admin/sign-in).
export default function SignOut() {
  const navigate = useNavigate();
  const client = useQueryClient();
  const write = useWrite();
  const me = useMe();
  const { pathname } = useLocation();
  const out = useMutation({
    mutationFn: () => write<void>("POST", "/auth/logout"),
    onSuccess: () => {
      client.clear();
      if (me.signIn === "jwt") navigate("/signed-out", { replace: true });
      // A local admin has no SSO button on /sign-in: they go back to theirs.
      else navigate(me.kind === "password" || (me.admin && inAdminArea(pathname)) ? "/admin/sign-in" : "/sign-in", { replace: true });
    },
  });
  return (
    <>
      <button type="button" className="linkish" onClick={() => out.mutate()} disabled={out.isPending}>
        Sign out
      </button>
      {out.isError && <ErrorBox error={out.error} />}
    </>
  );
}
