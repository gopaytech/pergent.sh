import { useQuery } from "@tanstack/react-query";
import { Navigate, useSearchParams } from "react-router";

import { ApiError, api, type Me, type SignInOptions } from "../api";
import { CenteredCard, ErrorBox, Loading } from "../ui";

// signInErrors are the messages for the codes the server puts in
// /sign-in?error=<code> when an SSO sign-in is refused. Only these codes show
// anything: the page never prints text it got from the address.
const signInErrors: Record<string, string> = {
  not_allowed: "Ask an admin to add you",
  expired: "Sign-in expired or was started in another window. Please try again.",
  failed: "Sign-in failed. Please try again.",
  denied: "Sign-in was not completed.",
  not_set_up: "Sign-in isn't set up yet. Ask your admin.",
  unavailable: "The sign-in provider can't be reached right now. Please try again.",
};

const jwtWords = "Your organisation signs you in before this page loads. Reload the page to try again, or ask your admin.";

// useSignInPage reads what both sign-in pages need: the options, the refusal
// the address carries, and whether the browser is signed in already (a quiet
// read of /api/me: its 401 is the usual answer here, not a reason to go to the
// sign-in page, which is this one). With JWT sign-in the page load itself
// signs the person in, so a signed-in browser is sent on.
//
// quiet is for the admin sign-in page: with JWT sign-in the proxy's header
// would sign the person in as themselves before a local admin could use the
// password form, so its read asks /api/me?probe=1, which starts no session.
export function useSignInPage(quiet = false) {
  const [params] = useSearchParams();
  const code = params.get("error") ?? "";
  const options = useQuery({
    queryKey: ["sign-in-options"],
    queryFn: () => api.get<SignInOptions>("/api/sign-in-options"),
  });
  const signedIn = useQuery({
    queryKey: ["signed-in"],
    queryFn: () => api.probe<Me>(quiet ? "/api/me?probe=1" : "/api/me"),
    retry: false,
    staleTime: 0,
  });
  const probeError = signedIn.error instanceof ApiError ? signedIn.error : null;
  // The words for a refusal: the address's code, else the proxy's.
  const refusal =
    (Object.hasOwn(signInErrors, code) ? signInErrors[code] : null) ??
    (probeError?.code === "not_allowed" ? signInErrors.not_allowed : null);
  return { options, signedIn, probeError, refusal };
}

// SignedInRedirect sends a signed-in browser on: an admin to /admin, a user to
// /.
export function SignedInRedirect({ me, admin }: { me: Me; admin: boolean }) {
  return <Navigate to={admin && me.admin ? "/admin" : "/"} replace />;
}

export function Refusal({ text }: { text: string | null }) {
  return (
    text && (
      <div className="error" role="alert">
        {text}
      </div>
    )
  );
}

// SignIn is the users' sign-in page: the SSO button, nothing else. It has no
// password form and no link to the admin one. With no SSO set up it says so;
// with JWT sign-in there is nothing to press (the page load signs the person
// in) and it says that.
export default function SignIn() {
  const { options, signedIn, probeError, refusal } = useSignInPage();
  if (signedIn.isSuccess) return <SignedInRedirect me={signedIn.data} admin={false} />;
  const sso = options.data?.sso ?? null;
  const jwt = options.data?.jwt === true;
  return (
    <CenteredCard>
      <h1>Sign in to pergent.sh</h1>
      <p className="sub">See why a merge request or pull request wasn't approved, and the repositories pergent reviews.</p>
      <Refusal text={refusal} />
      {options.isPending && <Loading />}
      {options.isError && <ErrorBox error={options.error} />}
      {options.isSuccess && sso && (
        <div className="signin-buttons">
          <a className="btn primary" href="/auth/oidc/login">
            Sign in with {sso.label}
          </a>
        </div>
      )}
      {options.isSuccess && jwt && !refusal && <p className="sub">{jwtWords}</p>}
      {jwt && probeError?.status === 503 && <ErrorBox error={probeError} />}
      {options.isSuccess && !sso && !jwt && <p className="sub">Sign-in isn't set up yet. Ask your admin.</p>}
    </CenteredCard>
  );
}
