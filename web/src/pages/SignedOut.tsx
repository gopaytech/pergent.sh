import { Link } from "react-router";

import { CenteredCard } from "../ui";

// SignedOut is where signing out of a JWT session ends. It asks the server
// nothing, and above all not /api/me: with the proxy's header on the request
// that would start a session again.
export default function SignedOut() {
  return (
    <CenteredCard>
      <h1>You're signed out</h1>
      <p className="sub">Your organisation's sign-in may sign you in again the next time you open pergent.sh.</p>
      <Link className="btn primary" to="/sign-in">
        Sign in again
      </Link>
    </CenteredCard>
  );
}
