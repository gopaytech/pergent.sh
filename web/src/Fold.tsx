import type { ReactNode } from "react";

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
  title: string;
  summary?: ReactNode;
  open: boolean;
  onToggle: () => void;
  children?: ReactNode;
}) {
  return (
    <h2 className="fold-head">
      <button type="button" className="fold" aria-expanded={open} data-fold="" onClick={onToggle}>
        <span className="fold-title">{title}</span>
        {summary !== undefined && summary !== "" && <span className="fold-sum">{summary}</span>}
      </button>
      {children}
    </h2>
  );
}

// countText is "1 model" or "3 models".
export const countText = (n: number, one: string, many = `${one}s`) => `${n} ${n === 1 ? one : many}`;
