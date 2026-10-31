import { useMutation, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";

import { api } from "../api";
import { useCSRFRetry } from "../layout/write";
import SignOut from "../layout/SignOut";
import { CenteredCard, ErrorBox } from "../ui";

// minLength is the server's rule (characters, not bytes); it is checked
// here too so a short one doesn't cost a round trip.
const minLength = 12;

// ChangePassword is all a password account sees until it has changed the
// password it was given: the server answers it nothing else.
export default function ChangePassword() {
  const client = useQueryClient();
  const withCSRF = useCSRFRetry();
  const [current, setCurrent] = useState("");
  const [next, setNext] = useState("");
  const [again, setAgain] = useState("");
  const [problem, setProblem] = useState("");
  const change = useMutation({
    mutationFn: () => withCSRF((csrf) => api.changePassword(current, next, csrf)),
    // The closure holds the passwords: drop it with the form, not after 5
    // minutes.
    gcTime: 0,
    // On success /api/me no longer says the password must change, and the
    // app opens. On failure it is read too: a 500 can follow a change that
    // was made, and the form would be dead.
    onSettled: (_data, err) => {
      if (err) {
        setCurrent("");
        setNext("");
        setAgain("");
      }
      return client.invalidateQueries({ queryKey: ["me"] });
    },
  });
  const submit = (e: FormEvent) => {
    e.preventDefault();
    change.reset();
    if ([...next].length < minLength) {
      setProblem(`The new password must have at least ${minLength} characters.`);
    } else if (next !== again) {
      setProblem("The new passwords don't match.");
    } else {
      setProblem("");
      change.mutate();
    }
  };
  return (
    <CenteredCard>
      <h1>Choose a new password</h1>
      <p className="sub">You must change the password you were given before you can use pergent.sh.</p>
      <form className="stack" onSubmit={submit}>
        <label htmlFor="current-password">Current password</label>
        <input
          id="current-password"
          type="password"
          autoComplete="current-password"
          value={current}
          onChange={(e) => setCurrent(e.target.value)}
          required
        />
        <label htmlFor="new-password">New password</label>
        <input
          id="new-password"
          type="password"
          autoComplete="new-password"
          value={next}
          onChange={(e) => setNext(e.target.value)}
          required
        />
        <label htmlFor="new-password-again">New password again</label>
        <input
          id="new-password-again"
          type="password"
          autoComplete="new-password"
          value={again}
          onChange={(e) => setAgain(e.target.value)}
          required
        />
        {problem && (
          <div className="error" role="alert">
            {problem}
          </div>
        )}
        {change.isError && <ErrorBox error={change.error} />}
        <button type="submit" className="btn primary" disabled={change.isPending}>
          Change password
        </button>
      </form>
      <SignOut />
    </CenteredCard>
  );
}
