import { useEffect, useState } from "react";
import { useSearchParams } from "react-router";

// useSearchText is the search as typed, and the search to send: the text in
// the address (?q=), set a moment after typing stops, so a reload or a shared
// link opens the same search and each key press is not a request.
export function useSearchText(): [string, (v: string) => void, string] {
  const [params, setParams] = useSearchParams();
  const sent = params.get("q") ?? "";
  const [typed, setTyped] = useState(sent);
  useEffect(() => {
    const t = typed.trim();
    if (t === sent) return;
    const timer = setTimeout(
      () =>
        setParams(
          (old) => {
            const next = new URLSearchParams(old);
            if (t === "") next.delete("q");
            else next.set("q", t);
            return next;
          },
          { replace: true },
        ),
      300,
    );
    return () => clearTimeout(timer);
  }, [typed, sent, setParams]);
  return [typed, setTyped, sent];
}
