import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link } from "react-router";

import { Button } from "@/components/ui/button";
import { Card, CardHeader, CardTitle } from "@/components/ui/card";

import { api, paths, type PastDismissalView } from "../api";
import { useWrite } from "../layout/write";
import { Ago, ErrorBox, Loading, Pill, changeRef } from "../ui";

// PastDismissals is what teaches this repository's reviews: the findings people
// dismissed with a reason in the last 90 days, one row each. Stop using ends the
// lesson for every dismissal of that finding; the dismissals stay on their
// changes. Everything but the skill is text people wrote.
export default function PastDismissals({
  instance,
  project,
  platform,
  learning,
}: {
  instance: string;
  project: string;
  platform: string;
  // learning: the repository's Learn from dismissals setting.
  learning: boolean;
}) {
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
    <div className="flex flex-col gap-4">
      <p className="text-muted-foreground text-sm">
        Findings people dismissed with a reason in the last 90 days.{" "}
        {learning ? "Reviews of this repository are told about them." : "Learn from dismissals is off, so reviews aren't told about these."}{" "}
        Stop using ends it for every dismissal of that finding; the dismissals stay on their changes.
      </p>
      {stop.isError && <ErrorBox error={stop.error} />}
      <Card role="region" aria-label="Past dismissals" className="gap-0 overflow-hidden py-0">
        <CardHeader className="py-3">
          <CardTitle>Past dismissals</CardTitle>
        </CardHeader>
        {rows.length === 0 && <div className="text-muted-foreground border-t px-4 py-6 text-sm">No past dismissals are teaching reviews.</div>}
        {rows.map((r) => (
          // "row" stays as a name the tests find a dismissal's row by;
          // border-b-0 drops the old row rule's bottom line.
          <div className="row flex items-start gap-3 border-t border-b-0 px-4 py-3" key={r.fingerprint}>
            <div className="flex min-w-0 grow flex-col gap-1 text-sm">
              <div className="flex items-center gap-2">
                <Pill tone={r.severity === "blocking" ? "bad" : "grey"}>{r.severity}</Pill> <span className="truncate font-semibold">{r.title}</span>
              </div>
              <div className="text-muted-foreground text-xs">
                {r.skill} · <span className="font-mono">{r.file}</span>
              </div>
              <div className="prose">{`Reason: ${r.reason}`}</div>
              <div className="text-muted-foreground text-xs">
                {r.by} · <Ago t={r.at} /> ·{" "}
                <Link className="text-primary hover:underline" to={paths.review(instance, project, r.number)}>
                  {changeRef(platform, project, r.number)}
                </Link>
              </div>
            </div>
            <Button
              type="button"
              variant="outline"
              size="sm"
              aria-label={`Stop using the dismissals of ${r.title}`}
              disabled={stop.isPending}
              onClick={() => {
                if (window.confirm(`Stop using the dismissals of "${r.title}"? Later reviews are no longer told about them. This can't be undone here.`)) {
                  stop.mutate(r.fingerprint);
                }
              }}
            >
              Stop using
            </Button>
          </div>
        ))}
      </Card>
    </div>
  );
}
