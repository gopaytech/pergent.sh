import { Link } from "react-router";

import { Badge } from "@/components/ui/badge";

import { paths, type ReviewSummary } from "../api";
import { Ago, OutcomePill, changeNumber, changeRef, platformName } from "../ui";

// attemptsText says "2 attempts" for a review that fell back to a backup
// model, and nothing for a review of one attempt.
const attemptsText = (n: number) => (n > 1 ? `${n} attempts` : "");

// ReviewRows lists reviews as the overview's "Recent reviews" does: the
// outcome, the change and its title, the reason, the attempts and when.
// inRepo is a repository's own list: its page already names the repository
// and its platform, so each row shows only the change number.
// Each row draws its own top line, so the list sits under a card's header.
export default function ReviewRows({ items, empty, inRepo }: { items: ReviewSummary[]; empty: string; inRepo?: boolean }) {
  if (items.length === 0) return <div className="text-muted-foreground border-t px-4 py-6 text-sm">{empty}</div>;
  return (
    <>
      {items.map((r) => (
        // "row" stays as a name the tests find a review's row by; border-b-0
        // drops the old row rule's bottom line.
        <div className="row flex items-center gap-3 border-t border-b-0 px-4 py-3" key={`${r.instance}/${r.project}/${r.number}`}>
          {/* The outcome sits in a column of its own width, so every title
              starts at the same place whatever the outcome's word. */}
          <div className="flex min-w-24 shrink-0">
            <OutcomePill outcome={r.outcome} />
          </div>
          <div className="min-w-0 grow">
            <Link className="text-foreground block truncate text-sm hover:underline" to={paths.review(r.instance, r.project, r.number)}>
              <span className="font-semibold">{inRepo ? changeNumber(r.platform, r.number) : changeRef(r.platform, r.project, r.number)}</span> · {r.title}
            </Link>
            <div className="text-muted-foreground mt-0.5 text-xs">
              {[r.reason, attemptsText(r.attempts)].filter(Boolean).map((part) => `${part} · `)}
              <Ago t={r.updatedAt} />
            </div>
          </div>
          {!inRepo && (
            <Badge variant="outline" className="text-muted-foreground">
              {platformName(r.platform)}
            </Badge>
          )}
        </div>
      ))}
    </>
  );
}
