import { useInfiniteQuery } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";
import { Link } from "react-router";

import { api, paths, query, type APIReviewList } from "../api";
import { ErrorBox, Loading, utcText } from "../ui";
import { StatusPill, VerdictPill } from "./ApiReview";

type Filter = { tenant: string; subject: string; status: string };

const none: Filter = { tenant: "", subject: "", status: "" };

// The statuses a review has: the ones the list takes.
const statuses: [string, string][] = [
  ["queued", "Queued"],
  ["running", "Running"],
  ["completed", "Completed"],
  ["failed", "Failed"],
  ["superseded", "Superseded"],
];

// ClientReviews lists a client's reviews, newest first, filtered by tenant,
// subject and status. The filter is sent when the admin applies it, not on
// every key.
export default function ClientReviews({ client }: { client: string }) {
  const [draft, setDraft] = useState<Filter>(none);
  const [applied, setApplied] = useState<Filter>(none);
  const list = useInfiniteQuery({
    queryKey: ["api-reviews", client, applied.tenant, applied.subject, applied.status],
    initialPageParam: "",
    queryFn: ({ pageParam }) =>
      api.get<APIReviewList>(
        paths.apiAdminClientReviews(client) +
          query([
            ["tenant", applied.tenant],
            ["subject", applied.subject],
            ["status", applied.status],
            ["cursor", pageParam],
          ]),
      ),
    getNextPageParam: (last) => last.nextCursor || undefined,
  });
  const submit = (e: FormEvent) => {
    e.preventDefault();
    setApplied({ ...draft, tenant: draft.tenant.trim(), subject: draft.subject.trim() });
  };
  const filtered = applied.tenant !== "" || applied.subject !== "" || applied.status !== "";
  const items = list.data?.pages.flatMap((p) => p.items) ?? [];
  return (
    <>
      <form className="filters" aria-label="Filter reviews" onSubmit={submit}>
        <input
          type="text"
          aria-label="Tenant"
          placeholder="Tenant"
          autoComplete="off"
          spellCheck={false}
          value={draft.tenant}
          onChange={(e) => setDraft({ ...draft, tenant: e.target.value })}
        />
        <input
          type="text"
          aria-label="Subject"
          placeholder="Subject"
          autoComplete="off"
          spellCheck={false}
          value={draft.subject}
          onChange={(e) => setDraft({ ...draft, subject: e.target.value })}
        />
        <select aria-label="Status" value={draft.status} onChange={(e) => setDraft({ ...draft, status: e.target.value })}>
          <option value="">Any status</option>
          {statuses.map(([value, label]) => (
            <option key={value} value={value}>
              {label}
            </option>
          ))}
        </select>
        <button type="submit" className="btn">
          Filter
        </button>
      </form>
      {list.isPending && <Loading />}
      {list.isError && <ErrorBox error={list.error} />}
      {list.isSuccess &&
        (items.length === 0 ? (
          <div className="card empty">{filtered ? "No reviews match." : "No reviews yet."}</div>
        ) : (
          <div className="card scroll">
            <table className="table">
              <thead>
                <tr>
                  <th>Subject</th>
                  <th>Tenant</th>
                  <th>Title</th>
                  <th>Status</th>
                  <th>Verdict</th>
                  <th>Created</th>
                  <th>Finished</th>
                </tr>
              </thead>
              <tbody>
                {items.map((r) => (
                  <tr key={r.id}>
                    <td>
                      <Link to={paths.clientReviewPage(client, r.id)}>{r.subject}</Link>
                    </td>
                    <td>{r.tenant}</td>
                    <td>{r.title}</td>
                    <td>
                      <StatusPill status={r.status} />
                    </td>
                    <td>{r.verdict && <VerdictPill verdict={r.verdict} />}</td>
                    <td className="nowrap">{utcText(r.createdAt)}</td>
                    <td className="nowrap">{r.finishedAt ? utcText(r.finishedAt) : ""}</td>
                  </tr>
                ))}
              </tbody>
            </table>
            {list.hasNextPage && (
              <div className="foot">
                <button type="button" className="btn" onClick={() => list.fetchNextPage()} disabled={list.isFetchingNextPage}>
                  Load more
                </button>
              </div>
            )}
          </div>
        ))}
    </>
  );
}
