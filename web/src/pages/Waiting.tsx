import { useMe } from "../layout/me";
import SignOut from "../layout/SignOut";
import { CenteredCard } from "../ui";

// Waiting is all an account on no team sees: the server answers it nothing
// else.
export default function Waiting() {
  const me = useMe();
  return (
    <CenteredCard>
      <h1>Waiting for a team</h1>
      <p className="sub">
        You're signed in as {me.username} on {me.instance}. An admin needs to add you to a team.
      </p>
      <SignOut />
    </CenteredCard>
  );
}
