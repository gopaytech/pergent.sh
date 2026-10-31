import { useInfiniteQuery } from "@tanstack/react-query";

import { api, query, type AuditPageView, type AuditView } from "../api";
import { ErrorBox, Loading } from "../ui";

// when is an RFC 3339 time as "2026-10-30 14:02 UTC" (the API sends UTC).
const when = (at: string) => `${at.slice(0, 16).replace("T", " ")} UTC`;

// what is an action like "team_created" as "Team created".
const what = (action: string) => {
  const text = action.replaceAll("_", " ");
  return text.charAt(0).toUpperCase() + text.slice(1);
};

// where is the repository an entry is about, if any.
const where = (e: AuditView) => (e.project ? `${e.instance}/${e.project}` : e.instance);

// AuditLog is the record of who changed what, newest first; Older reads the
// page before the last one shown.
export default function AuditLog() {
  const log = useInfiniteQuery({
    queryKey: ["audit"],
    queryFn: ({ pageParam }) => api.get<AuditPageView>(`/api/audit${query([["before", pageParam || undefined]])}`),
    initialPageParam: 0,
    // A next of 0 is the end of the log.
    getNextPageParam: (last) => last.next || undefined,
  });
  // A failed Older leaves the query in error with the pages already read:
  // they stay, and so does the button to try again.
  const entries = log.data?.pages.flatMap((p) => p.entries) ?? [];
  return (
    <div className="narrow">
      <div className="top">
        <div>
          <h1>Audit log</h1>
          <div className="sub">Every change made in the dashboard, kept for 90 days. Never a secret.</div>
        </div>
      </div>
      {log.isPending && <Loading />}
      {log.isError && !log.isFetchNextPageError && <ErrorBox error={log.error} />}
      {log.data &&
        (entries.length === 0 && !log.hasNextPage ? (
          <div className="card empty">Nothing is recorded yet.</div>
        ) : (
          <div className="card scroll">
            <table className="table">
              <thead>
                <tr>
                  <th>When</th>
                  <th>Who</th>
                  <th>What</th>
                  <th>Where</th>
                  <th>Detail</th>
                </tr>
              </thead>
              <tbody>
                {entries.map((e) => (
                  <tr key={e.id}>
                    <td className="nowrap">{when(e.at)}</td>
                    <td>{e.actor}</td>
                    <td>{what(e.action)}</td>
                    <td className="mr">{where(e)}</td>
                    <td>{e.detail}</td>
                  </tr>
                ))}
              </tbody>
            </table>
            {log.hasNextPage && (
              <div className="foot">
                <button type="button" className="btn" disabled={log.isFetchingNextPage} onClick={() => log.fetchNextPage()}>
                  Older
                </button>
              </div>
            )}
          </div>
        ))}
      {log.isFetchNextPageError && <ErrorBox error={log.error} />}
    </div>
  );
}
