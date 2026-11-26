import { useMe } from "../layout/me";
import SignOut from "../layout/SignOut";
import { CenteredCard } from "../ui";
import { SignInHead } from "./SignIn";

// Waiting is all an account on no team sees: the server answers it nothing
// else.
export default function Waiting() {
  const me = useMe();
  return (
    <CenteredCard>
      <SignInHead title="Waiting for a team">
        You're signed in as {me.email || me.username}. An admin needs to add you to a team.
      </SignInHead>
      <SignOut />
    </CenteredCard>
  );
}
