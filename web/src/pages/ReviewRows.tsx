import { Link } from "react-router";

import { paths, type ReviewSummary } from "../api";
import { OutcomePill, ago, changeRef, platformName } from "../ui";

// ReviewRows lists reviews as the overview's "Recent reviews" does: the
// outcome, the change and its title, the reason and when.
export default function ReviewRows({ items, empty }: { items: ReviewSummary[]; empty: string }) {
  if (items.length === 0) return <div className="row empty-row">{empty}</div>;
  return (
    <>
      {items.map((r) => (
        <div className="row" key={`${r.instance}/${r.project}/${r.number}`}>
          <OutcomePill outcome={r.outcome} />
          <div className="grow">
            <Link className="title" to={paths.review(r.instance, r.project, r.number)}>
              <span className="mr">{changeRef(r.platform, r.project, r.number)}</span> · {r.title}
            </Link>
            <div className="meta">{[r.reason, ago(r.updatedAt)].filter(Boolean).join(" · ")}</div>
          </div>
          <span className="plat">{platformName(r.platform)}</span>
        </div>
      ))}
    </>
  );
}
