import { useMutation, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";
import { useNavigate } from "react-router";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";

import { api } from "../api";
import { CenteredCard, ErrorBox, Loading } from "../ui";
import { Refusal, SSOButton, SignInHead, SignedInRedirect, useSignInPage } from "./SignIn";

// AdminSignIn is the admins' sign-in page: the password form of the local
// admins, and the SSO button for an SSO account an admin made an admin. With
// JWT validation this page starts no session, so that a local admin behind the
// proxy can reach the form: a plain link to /admin is the way on for anyone
// else, because a page load there does start one. A signed-in admin lands on
// /admin and a user on /.
export default function AdminSignIn() {
  const { options, signedIn, refusal } = useSignInPage(true);
  if (signedIn.isSuccess) return <SignedInRedirect me={signedIn.data} admin />;
  const sso = options.data?.sso ?? null;
  return (
    <CenteredCard>
      <SignInHead title="Admin sign-in">For admins of pergent.sh. Everyone else signs in at the usual page.</SignInHead>
      <Refusal text={refusal} />
      {options.isPending && <Loading />}
      {options.isError && <ErrorBox error={options.error} />}
      {options.isSuccess && options.data.password && <PasswordForm />}
      {options.isSuccess && sso && (
        <>
          {options.data.password && <Or />}
          <SSOButton label={sso.label} />
        </>
      )}
      {options.isSuccess && options.data.jwt && (
        <>
          {options.data.password && <Or />}
          <p className="text-muted-foreground text-sm">
            Your organisation signs you in, so this page doesn't.{" "}
            <a className="text-primary font-medium underline-offset-4 hover:underline" href="/admin">
              Go to the admin pages
            </a>
            . If you come back here, ask your admin.
          </p>
        </>
      )}
    </CenteredCard>
  );
}

// Or stands between two ways to sign in.
function Or() {
  return <div className="text-muted-foreground text-center text-xs">or</div>;
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
    <form className="flex flex-col gap-4" onSubmit={submit}>
      <div className="grid gap-2">
        <Label htmlFor="signin-username">Username</Label>
        <Input
          id="signin-username"
          type="text"
          autoComplete="username"
          autoCapitalize="none"
          spellCheck={false}
          value={username}
          onChange={(e) => setUsername(e.target.value)}
          required
        />
      </div>
      <div className="grid gap-2">
        <Label htmlFor="signin-password">Password</Label>
        <Input
          id="signin-password"
          type="password"
          autoComplete="current-password"
          value={password}
          onChange={(e) => setPassword(e.target.value)}
          required
        />
      </div>
      {signIn.isError && <ErrorBox error={signIn.error} />}
      <Button type="submit" className="w-full" disabled={signIn.isPending}>
        Sign in
      </Button>
    </form>
  );
}

