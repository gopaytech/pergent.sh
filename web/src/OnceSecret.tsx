import type { ReactNode } from "react";

import { CopyButton } from "./ui";

// OnceSecret shows a secret the server made and will not show again: a key,
// a callback secret. It is a labelled region, not a live region, so a screen
// reader doesn't read the secret out unasked. title names the region and what
// names the copy button ("Copy <what>"); children add a line below. The
// caller drops the answer with the box (a mutation with gcTime 0 and a
// reset in onDone).
export default function OnceSecret({
  title,
  what,
  secret,
  onDone,
  children,
}: {
  title: string;
  what: string;
  secret: string;
  onDone: () => void;
  children?: ReactNode;
}) {
  return (
    <section className="card pad secret-box" aria-label={title}>
      <code className="secret-value">{secret}</code>
      <p>Copy this secret now: it is not shown again.</p>
      {children}
      <div className="actions-row">
        <CopyButton text={secret} what={what} />
        <button type="button" className="btn primary" onClick={onDone}>
          Done
        </button>
      </div>
    </section>
  );
}
