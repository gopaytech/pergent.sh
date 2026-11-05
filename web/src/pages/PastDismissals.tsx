import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link } from "react-router";

import { api, paths, type PastDismissalView } from "../api";
import { useWrite } from "../layout/write";
import { ErrorBox, Loading, Pill, ago, changeRef } from "../ui";

// PastDismissals is what teaches this repository's reviews: the findings people
// dismissed with a reason in the last 90 days, one row each. Stop using ends the
// lesson for every dismissal of that finding; the dismissals stay on their
// changes. Everything but the skill is text people wrote.
export default function PastDismissals({ instance, project, platform }: { instance: string; project: string; platform: string }) {
  const client = useQueryClient();
  const write = useWrite();
  const key = ["past-dismissals", instance, project];
  const list = useQuery({
    queryKey: key,
    queryFn: () => api.get<PastDismissalView[]>(paths.apiRepoPastDismissals(instance, project)),
  });
  const stop = useMutation({
    mutationFn: (fingerprint: string) =>
      write<{ status: string }>("POST", paths.apiRepoPastDismissalStop(instance, project), { fingerprint }),
    onSuccess: () => client.invalidateQueries({ queryKey: key }),
  });

  if (list.isPending) return <Loading />;
  if (list.isError) return <ErrorBox error={list.error} />;
  const rows = list.data;

  return (
    <>
      <p className="sub">
        Findings people dismissed with a reason in the last 90 days. Reviews of this repository are told about them. Stop using
        ends it for every dismissal of that finding; the dismissals stay on their changes.
      </p>
      {stop.isError && <ErrorBox error={stop.error} />}
      <section className="card" aria-label="Past dismissals">
        <h2>Past dismissals</h2>
        {rows.length === 0 && <div className="row empty-row">No past dismissals are teaching reviews.</div>}
        {rows.map((r) => (
          <div className="row" key={r.fingerprint}>
            <div className="grow">
              <div className="title">
                <Pill tone={r.severity === "blocking" ? "bad" : "grey"}>{r.severity}</Pill> <span className="t">{r.title}</span>
              </div>
              <div className="meta">
                {r.skill} · <span className="mono">{r.file}</span>
              </div>
              <div className="prose">{`Reason: ${r.reason}`}</div>
              <div className="meta">
                {r.by} · {ago(r.at)} · <Link to={paths.review(instance, project, r.number)}>{changeRef(platform, project, r.number)}</Link>
              </div>
            </div>
            <button type="button" className="btn small" disabled={stop.isPending} onClick={() => stop.mutate(r.fingerprint)}>
              Stop using
            </button>
          </div>
        ))}
      </section>
    </>
  );
}
