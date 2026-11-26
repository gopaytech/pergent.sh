import { createContext, useContext, useState, type ReactNode } from "react";

import { ChevronRight } from "lucide-react";

import { cn } from "@/lib/utils";

// FoldHead is a card's title as a button that folds and unfolds the card's
// body, with a one-line summary of what is inside, so a long page reads at
// a glance while every card is folded. children are buttons that sit on
// the right of the title, such as Add model.
export function FoldHead({
  title,
  summary,
  open,
  onToggle,
  children,
}: {
  title: ReactNode;
  summary?: ReactNode;
  open: boolean;
  onToggle: () => void;
  children?: ReactNode;
}) {
  return (
    <h2 className="flex items-center gap-2 pr-4 text-[15px] leading-tight font-semibold">
      <button
        type="button"
        className="focus-visible:ring-ring/50 flex min-w-0 flex-1 cursor-pointer items-center gap-2 rounded-xl px-4 py-3 text-left outline-none focus-visible:ring-[3px]"
        aria-expanded={open}
        data-fold=""
        onClick={onToggle}
      >
        <ChevronRight
          className={cn("text-muted-foreground size-4 shrink-0 transition-transform", open && "rotate-90")}
          aria-hidden="true"
        />
        <span className="flex shrink-0 items-center gap-1.5">{title}</span>
        {summary !== undefined && summary !== "" && (
          <span className="text-muted-foreground min-w-0 truncate text-sm font-normal">{summary}</span>
        )}
      </button>
      {children}
    </h2>
  );
}

// countText is "1 model" or "3 models".
export const countText = (n: number, one: string, many = `${one}s`) => `${n} ${n === 1 ? one : many}`;

// FoldsStartOpen starts every FoldCard open. The app never sets it; tests
// about what is inside the cards do (renderApp's unfolded), so each test
// need not unfold the card it reads first.
export const FoldsStartOpen = createContext(false);

// FoldCard is a card whose title folds it, open or folded at first as
// defaultOpen says. A folded card keeps its body mounted, only hidden, so
// what is typed in it stays; data-fold-card lets a page open the card that
// holds a field it has to show (openFoldOf).
export function FoldCard({
  label,
  title,
  summary,
  defaultOpen = false,
  actions,
  children,
}: {
  label: string;
  title: ReactNode;
  summary?: ReactNode;
  defaultOpen?: boolean;
  actions?: ReactNode;
  children: ReactNode;
}) {
  const startOpen = useContext(FoldsStartOpen);
  const [open, setOpen] = useState(defaultOpen || startOpen);
  return (
    // data-slot="card" marks it as a card, as the Card component does;
    // "folded" is what openFoldOf looks for.
    <section
      className={cn(
        "bg-card text-card-foreground mb-4 overflow-hidden rounded-xl border shadow-xs",
        !open && "folded",
      )}
      aria-label={label}
      data-fold-card=""
      data-slot="card"
    >
      <FoldHead title={title} summary={summary} open={open} onToggle={() => setOpen(!open)}>
        {actions}
      </FoldHead>
      <div className="border-t" hidden={!open}>
        {children}
      </div>
    </section>
  );
}

// openFoldOf opens the folded card that holds el, if it is in one, and says
// whether it did: the card shows on the next render, so focus waits for it.
export function openFoldOf(el: Element | null): boolean {
  const card = el?.closest("[data-fold-card].folded");
  const button = card?.querySelector<HTMLButtonElement>(":scope > h2 [data-fold]");
  if (!button) return false;
  button.click();
  return true;
}
