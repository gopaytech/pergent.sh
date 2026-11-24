import { Link } from "react-router";

import { paths, type ReviewSummary } from "../api";
import { OutcomePill, ago, changeNumber, changeRef, platformName } from "../ui";

// attemptsText says "2 attempts" for a review that fell back to a backup
// model, and nothing for a review of one attempt.
const attemptsText = (n: number) => (n > 1 ? `${n} attempts` : "");

// ReviewRows lists reviews as the overview's "Recent reviews" does: the
// outcome, the change and its title, the reason, the attempts and when.
// inRepo is a repository's own list: its page already names the repository
// and its platform, so each row shows only the change number.
export default function ReviewRows({ items, empty, inRepo }: { items: ReviewSummary[]; empty: string; inRepo?: boolean }) {
  if (items.length === 0) return <div className="row empty-row">{empty}</div>;
  return (
    <>
      {items.map((r) => (
        <div className="row review-row" key={`${r.instance}/${r.project}/${r.number}`}>
          <OutcomePill outcome={r.outcome} />
          <div className="grow">
            <Link className="title" to={paths.review(r.instance, r.project, r.number)}>
              <span className="mr">{inRepo ? changeNumber(r.platform, r.number) : changeRef(r.platform, r.project, r.number)}</span> · {r.title}
            </Link>
            <div className="meta">{[r.reason, attemptsText(r.attempts), ago(r.updatedAt)].filter(Boolean).join(" · ")}</div>
          </div>
          {!inRepo && <span className="plat">{platformName(r.platform)}</span>}
        </div>
      ))}
    </>
  );
}
