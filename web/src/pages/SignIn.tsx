import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";
import { useNavigate, useSearchParams } from "react-router";

import { api, paths, type InstancesView } from "../api";
import { CenteredCard, ErrorBox, Loading } from "../ui";

const platforms = new Set(["gitlab", "github"]);

// signInErrors are the messages for the codes the server puts in
// /signin?error=<code> when an SSO sign-in is refused. Only these codes show
// anything: the page never prints text it got from the address.
const signInErrors: Record<string, string> = {
  not_allowed: "Ask an admin to add you",
  expired: "Sign-in expired or was started in another window. Please try again.",
  failed: "Sign-in failed. Please try again.",
  denied: "Sign-in was not completed.",
  not_set_up: "Sign-in isn't set up yet. Ask your admin.",
  unavailable: "The sign-in provider can't be reached right now. Please try again.",
};

// SignIn has the password form, and the SSO button when OIDC is set up. The
// button goes to the server's login route, which sends the browser on to the
// identity provider and back.
export default function SignIn() {
  const [params] = useSearchParams();
  const code = params.get("error") ?? "";
  const refusal = Object.hasOwn(signInErrors, code) ? signInErrors[code] : null;
  const instances = useQuery({
    queryKey: ["instances"],
    queryFn: () => api.get<InstancesView>("/api/instances"),
  });
  const usable = (instances.data?.instances ?? []).filter((i) => platforms.has(i.platform));
  const sso = instances.data?.sso ?? null;
  const nothing = instances.isError || (instances.isSuccess && !instances.data.password && usable.length === 0 && !sso);
  return (
    <CenteredCard>
      <h1>Sign in to pergent.sh</h1>
      <p className="sub">See why a merge request or pull request wasn't approved, and manage the repositories pergent reviews.</p>
      {refusal && (
        <div className="error" role="alert">
          {refusal}
        </div>
      )}
      {instances.isPending && <Loading />}
      {instances.isError && <ErrorBox error={instances.error} />}
      {instances.isSuccess && instances.data.password && <PasswordForm />}
      {nothing && <p className="sub">No sign-in is configured.</p>}
      {(usable.length > 0 || sso) && (
        <>
          {instances.data?.password && <div className="signin-or">or</div>}
          <div className="signin-buttons">
            {sso && (
              <a className="btn primary" href="/auth/oidc/login">
                Sign in with {sso.label}
              </a>
            )}
            {usable.map((i) => (
              <a key={`${i.platform}/${i.instance}`} className="btn primary" href={paths.login(i.platform, i.instance)}>
                Sign in with {i.label}
              </a>
            ))}
          </div>
        </>
      )}
    </CenteredCard>
  );
}

function PasswordForm() {
  const client = useQueryClient();
  const navigate = useNavigate();
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const signIn = useMutation({
    mutationFn: () => api.signIn(username, password),
    // The closure holds the password: drop it with the form, not after 5
    // minutes.
    gcTime: 0,
    onSuccess: () => {
      // Nothing loaded before belongs to this session.
      client.clear();
      navigate("/", { replace: true });
    },
    // A wrong password is typed again; the username stays.
    onError: () => setPassword(""),
  });
  const submit = (e: FormEvent) => {
    e.preventDefault();
    signIn.mutate();
  };
  return (
    <form className="stack" onSubmit={submit}>
      <label htmlFor="signin-username">Username</label>
      <input
        id="signin-username"
        type="text"
        autoComplete="username"
        autoCapitalize="none"
        spellCheck={false}
        value={username}
        onChange={(e) => setUsername(e.target.value)}
        required
      />
      <label htmlFor="signin-password">Password</label>
      <input
        id="signin-password"
        type="password"
        autoComplete="current-password"
        value={password}
        onChange={(e) => setPassword(e.target.value)}
        required
      />
      {signIn.isError && <ErrorBox error={signIn.error} />}
      <button type="submit" className="btn primary" disabled={signIn.isPending}>
        Sign in
      </button>
    </form>
  );
}
