import { useMutation, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";
import { useNavigate } from "react-router";

import { api } from "../api";
import { CenteredCard, ErrorBox, Loading } from "../ui";
import { Refusal, SignedInRedirect, useSignInPage } from "./SignIn";

// AdminSignIn is the admins' sign-in page: the password form of the local
// admins, and the SSO button for an SSO account an admin made an admin. A
// signed-in admin lands on /admin and a user on /.
export default function AdminSignIn() {
  const { options, signedIn, refusal } = useSignInPage();
  if (signedIn.isSuccess) return <SignedInRedirect me={signedIn.data} admin />;
  const sso = options.data?.sso ?? null;
  return (
    <CenteredCard>
      <h1>Admin sign-in</h1>
      <p className="sub">For admins of pergent.sh. Everyone else signs in at the usual page.</p>
      <Refusal text={refusal} />
      {options.isPending && <Loading />}
      {options.isError && <ErrorBox error={options.error} />}
      {options.isSuccess && options.data.password && <PasswordForm />}
      {options.isSuccess && sso && (
        <>
          {options.data.password && <div className="signin-or">or</div>}
          <div className="signin-buttons">
            <a className="btn primary" href="/auth/oidc/login">
              Sign in with {sso.label}
            </a>
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
      navigate("/admin", { replace: true });
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

