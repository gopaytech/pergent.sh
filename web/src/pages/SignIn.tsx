import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";
import { useNavigate } from "react-router";

import { api, paths, type InstancesView } from "../api";
import { CenteredCard, ErrorBox, Loading } from "../ui";

const platforms = new Set(["gitlab", "github"]);

// SignIn has the password form, and one button per instance with sign-in.
// Each button goes to the server's login route, which sends the browser on
// to GitLab or GitHub.
export default function SignIn() {
  const instances = useQuery({
    queryKey: ["instances"],
    queryFn: () => api.get<InstancesView>("/api/instances"),
  });
  const usable = (instances.data?.instances ?? []).filter((i) => platforms.has(i.platform));
  const nothing = instances.isError || (instances.isSuccess && !instances.data.password && usable.length === 0);
  return (
    <CenteredCard>
      <h1>Sign in to pergent.sh</h1>
      <p className="sub">See why a merge request or pull request wasn't approved, and manage the repositories pergent reviews.</p>
      {instances.isPending && <Loading />}
      {instances.isError && <ErrorBox error={instances.error} />}
      {instances.isSuccess && instances.data.password && <PasswordForm />}
      {nothing && <p className="sub">No sign-in is configured.</p>}
      {usable.length > 0 && (
        <>
          {instances.data?.password && <div className="signin-or">or</div>}
          <div className="signin-buttons">
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
