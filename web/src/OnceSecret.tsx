import type { ReactNode } from "react";

import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";

import { CopyButton } from "./ui";

// secretCard is the look of a box that shows a secret once: amber, so it
// stands out from the cards around it. secretValue is the secret itself,
// selected whole on a click.
export const secretCard = "border-warn/25 bg-warn-soft text-warn mb-4 gap-3 px-4 py-4 text-sm";
export const secretValue = "bg-card border-warn/25 block rounded-md border px-3 py-2.5 font-mono text-[13px] break-all select-all text-foreground";

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
    <Card role="region" className={secretCard} aria-label={title}>
      <code className={secretValue}>{secret}</code>
      <p>Copy this secret now: it is not shown again.</p>
      {children}
      <div className="flex gap-2">
        <CopyButton text={secret} what={what} />
        <Button type="button" size="sm" onClick={onDone}>
          Done
        </Button>
      </div>
    </Card>
  );
}
