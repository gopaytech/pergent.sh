import { useMutation, useQueryClient } from "@tanstack/react-query";
import { useNavigate } from "react-router";

import { ErrorBox } from "../ui";
import { useWrite } from "./write";

// SignOut ends the session. Only once the server says it has ended does
// the page drop what it loaded and go to sign-in; a failure is shown and
// the user stays signed in, and knows it.
export default function SignOut() {
  const navigate = useNavigate();
  const client = useQueryClient();
  const write = useWrite();
  const out = useMutation({
    mutationFn: () => write<void>("POST", "/auth/logout"),
    onSuccess: () => {
      client.clear();
      navigate("/signin", { replace: true });
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
