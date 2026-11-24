import { useEffect, useRef } from "react";
import { useBlocker } from "react-router";

// leaveText is what the browser asks before unsaved changes are dropped.
export const leaveText = "You have unsaved changes. Leave and lose them?";

// useUnsavedGuard asks before the page is left while dirty: a link, a tab
// change, Back, a reload or closing the window. A move to a sign-in page (a
// 401 sends the browser there) is never held. Only one page component holds
// it at a time: a router keeps one blocker. It returns allow, for a page
// that moves on by itself once the draft is saved (a new skill opens its
// page): the move happens before the page sees the draft as saved.
export function useUnsavedGuard(dirty: boolean): () => void {
  const allowed = useRef(false);
  const blocker = useBlocker(
    ({ currentLocation, nextLocation }) =>
      dirty &&
      !allowed.current &&
      !nextLocation.pathname.endsWith("sign-in") &&
      (currentLocation.pathname !== nextLocation.pathname || currentLocation.search !== nextLocation.search),
  );
  useEffect(() => {
    if (blocker.state !== "blocked") return;
    if (window.confirm(leaveText)) blocker.proceed();
    else blocker.reset();
  }, [blocker]);
  useEffect(() => {
    if (!dirty) return;
    const warn = (e: BeforeUnloadEvent) => {
      e.preventDefault();
      // Older browsers ask only when returnValue is set.
      e.returnValue = "";
    };
    window.addEventListener("beforeunload", warn);
    return () => window.removeEventListener("beforeunload", warn);
  }, [dirty]);
  return () => {
    allowed.current = true;
  };
}
