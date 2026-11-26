import { Link } from "react-router";

import { Button } from "@/components/ui/button";

import { CenteredCard } from "../ui";
import { SignInHead } from "./SignIn";

// SignedOut is where signing out of a JWT session ends. It asks the server
// nothing, and above all not /api/me: with the proxy's header on the request
// that would start a session again.
export default function SignedOut() {
  return (
    <CenteredCard>
      <SignInHead title="You're signed out">
        Your organisation's sign-in may sign you in again the next time you open pergent.sh.
      </SignInHead>
      <Button asChild className="w-full">
        <Link to="/sign-in">Sign in again</Link>
      </Button>
    </CenteredCard>
  );
}
