import { useQuery } from "@tanstack/react-query";
import { CheckIcon, CircleIcon } from "lucide-react";
import { Link } from "react-router";

import { Button } from "@/components/ui/button";
import { Card, CardAction, CardHeader, CardTitle } from "@/components/ui/card";
import { NativeSelect } from "@/components/ui/native-select";
import { cn } from "@/lib/utils";

import { api, type OverviewView, type SetupView } from "../api";
import { useMe } from "../layout/me";
import { ErrorBox, Loading, PageHeader } from "../ui";
import ReviewRows from "./ReviewRows";
import { useUrlParam } from "../urlState";

const periods = [7, 30, 90];

// Overview is the landing page: the numbers of the last days and the recent
// reviews. A repository's setup problems are under its Setup checks tab.
export default function Overview() {
  const me = useMe();
  const [days, setDays] = useUrlParam<number>("days", 7, periods);
  const data = useQuery({
    queryKey: ["overview", days],
    queryFn: () => api.get<OverviewView>(`/api/overview?days=${days}`),
  });
  return (
    <>
      <PageHeader
        title="Overview"
        description={`Last ${days} days across the repositories you can see`}
        actions={
          <>
            <NativeSelect aria-label="Period" className="w-40" value={days} onChange={(e) => setDays(Number(e.target.value))}>
              {periods.map((d) => (
                <option key={d} value={d}>
                  Last {d} days
                </option>
              ))}
            </NativeSelect>
            {me.admin && (
              <Button asChild>
                <Link to="/repos?add=1">+ Add repository</Link>
              </Button>
            )}
          </>
        }
      />
      {data.isPending && <Loading />}
      {data.isError && <ErrorBox error={data.error} onRetry={() => void data.refetch()} />}
      {data.isSuccess && <Body view={data.data} days={days} />}
    </>
  );
}

function Body({ view, days }: { view: OverviewView; days: number }) {
  const { stats } = view;
  const pct = stats.reviews > 0 ? Math.round((stats.approved / stats.reviews) * 100) : 0;
  // Each number opens the reviews behind it, over the same days.
  const reviews = (outcome?: string) => `/reviews?${outcome ? `outcome=${outcome}&` : ""}days=${days}`;
  return (
    <div className="flex flex-col gap-6">
      {view.setup && <Checklist setup={view.setup} />}
      <section className="grid grid-cols-1 gap-4 sm:grid-cols-2 xl:grid-cols-4" aria-label="Numbers">
        <Stat k="Reviews" v={stats.reviews} d="not counting superseded or skipped runs" to={reviews()} />
        <Stat k="Approved by pergent" v={stats.approved} d={`${pct}% of reviews`} to={reviews("approved")} />
        <Stat k="Blocking findings" v={stats.blocking} d="reviews with blocking issues" to={reviews("blocking")} />
        <Stat
          k="Failed reviews"
          v={stats.failed}
          d={stats.failed > 0 ? "open them to see why" : "none failed"}
          bad={stats.failed > 0}
          to={stats.failed > 0 ? reviews("failed") : undefined}
        />
      </section>
      <Card role="region" aria-label="Recent reviews" className="gap-0 overflow-hidden py-0">
        <CardHeader className="py-3">
          <CardTitle>Recent reviews</CardTitle>
          <CardAction>
            <Link className="text-primary text-sm font-medium hover:underline" to={reviews()}>
              View all →
            </Link>
          </CardAction>
        </CardHeader>
        <ReviewRows items={view.recent} empty="No reviews yet." />
      </Card>
    </div>
  );
}

// The setup steps of a new install, in order, each with the page that does it.
const setupSteps: [keyof SetupView, string, string][] = [
  ["llm", "Set the model and its key", "/admin/settings"],
  ["instance", "Connect a GitLab or GitHub instance", "/admin/integrations"],
  ["skill", "Add a skill to the library", "/skills"],
  ["repository", "Switch on a repository", "/repos?add=1"],
];

// Checklist shows an admin what is left to set up. The overview sends the
// setup only while something is left, so it goes away once all four are done.
function Checklist({ setup }: { setup: SetupView }) {
  return (
    <Card role="region" aria-label="Setup checklist" className="gap-0 overflow-hidden py-0">
      <CardHeader className="py-3">
        <CardTitle>Get started</CardTitle>
      </CardHeader>
      <ul>
        {setupSteps.map(([key, label, to]) => (
          // "done" stays as a class name: the tests read a step's state from it.
          <li key={key} className={cn("flex items-center gap-2.5 border-t px-4 py-2.5 text-sm", setup[key] && "done")}>
            <span
              aria-label={setup[key] ? "done" : "to do"}
              role="img"
              className={cn("flex size-4 shrink-0 items-center justify-center", setup[key] ? "text-ok" : "text-muted-foreground")}
            >
              {setup[key] ? <CheckIcon className="size-4" aria-hidden="true" /> : <CircleIcon className="size-3.5" aria-hidden="true" />}
            </span>
            <Link
              className={setup[key] ? "text-muted-foreground line-through" : "text-primary font-medium hover:underline"}
              to={to}
            >
              {label}
            </Link>
          </li>
        ))}
      </ul>
    </Card>
  );
}

// Stat is one number as a card; with to, the whole card opens the reviews
// behind it.
function Stat({ k, v, d, bad, to }: { k: string; v: number; d: string; bad?: boolean; to?: string }) {
  const body = (
    <Card className={cn("h-full gap-1 px-4 py-4", to && "group-hover:border-primary transition-colors")}>
      <div className="text-muted-foreground text-sm">{k}</div>
      <div className="text-3xl font-semibold tracking-tight tabular-nums">{v}</div>
      <div className={cn("text-xs", bad ? "text-bad" : "text-muted-foreground")}>{d}</div>
    </Card>
  );
  return to ? (
    <Link
      className="group focus-visible:ring-ring/50 block rounded-xl outline-none focus-visible:ring-[3px]"
      to={to}
      aria-label={`${k}: ${v}. Open these reviews`}
    >
      {body}
    </Link>
  ) : (
    body
  );
}
