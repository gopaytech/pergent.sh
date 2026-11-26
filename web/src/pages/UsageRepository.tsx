import { useQuery } from "@tanstack/react-query";
import { Link, useParams } from "react-router";

import { Card, CardHeader, CardTitle } from "@/components/ui/card";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";

import { api, isStatus, paths, safeHref, type UsageNamedView, type UsageRepositoryView } from "../api";
import { ErrorBox, ExternalLink, Loading, NotFound, PageHeader, count } from "../ui";
import {
  CsvLink,
  DailyTable,
  dash,
  EmptyRow,
  PeriodSelect,
  periods,
  num,
  UsageCard,
  UsageCells,
  UsageHead,
  UsageNotes,
  reviewsText,
  sourceText,
  usageTable,
} from "./usageParts";
import { useUrlParam } from "../urlState";

// when is an RFC 3339 time as "2026-10-30 14:02 UTC" (the API sends UTC).
const when = (at: string) => `${at.slice(0, 16).replace("T", " ")} UTC`;

// median is a median of tokens, or a dash when there was no review of that kind.
const median = (m: number | null) => (m === null ? dash : count(Math.round(m)));

// modelText is a model as "provider / model", the name alone when the
// reviews recorded no provider.
const modelText = (m: { provider: string; model: string }) => (m.provider === "" ? m.model : `${m.provider} / ${m.model}`);

// UsageRepository is one repository's usage, for admins: what global and team
// keys paid, per skill, merge request, model and day, what was wasted and
// what a review costs. Titles, skill and model names are text only.
export default function UsageRepository() {
  const params = useParams();
  const instance = params.instance ?? "";
  const project = params["*"] ?? "";
  const [days, setDays] = useUrlParam<number>("days", 30, periods);
  const data = useQuery({
    queryKey: ["usage", instance, project, days],
    queryFn: () => api.get<UsageRepositoryView>(paths.apiRepoUsage(instance, project, days)),
    enabled: project !== "",
  });
  // A repository that is not switched on has no page: the API answers 404.
  if (project === "" || isStatus(data.error, 404)) return <NotFound />;
  return (
    <>
      <div className="text-muted-foreground mb-2 text-sm">
        <Link className="text-muted-foreground hover:text-foreground hover:underline" to="/admin/usage">
          Usage
        </Link>{" "}
        / {project}
      </div>
      <PageHeader
        title={project}
        description={`${instance} · the last ${days} days`}
        actions={
          <>
            <PeriodSelect days={days} onChange={setDays} />
            <CsvLink days={days} instance={instance} project={project} />
          </>
        }
      />
      <UsageNotes />
      {data.isPending && <Loading />}
      {data.isError && <ErrorBox error={data.error} />}
      {data.isSuccess && <Body view={data.data} instance={instance} project={project} />}
    </>
  );
}

function Body({ view, instance, project }: { view: UsageRepositoryView; instance: string; project: string }) {
  const { wasted } = view;
  return (
    <div className="flex flex-col gap-6">
      <p className="text-muted-foreground text-sm">{sourceText(view.globalTokens, view.teamTokens)}</p>
      <Named title="Skills" first="Skill" rows={view.skills} failed />
      <UsageCard label="Merge requests">
        {view.mergeRequests.length === 0 ? (
          <EmptyRow>No usage in this period.</EmptyRow>
        ) : (
          <Table className={usageTable}>
            <TableHeader className="bg-muted/40">
              <UsageHead first="Merge request">
                <TableHead>Models</TableHead>
                <TableHead className={num}>Reviews</TableHead>
              </UsageHead>
            </TableHeader>
            <TableBody>
              {view.mergeRequests.map((m) => (
                <TableRow key={m.iid}>
                  <TableCell>
                    <Link className="text-primary font-medium hover:underline" to={paths.review(instance, project, m.iid)}>
                      {m.iid}
                    </Link>{" "}
                    {safeHref(m.webURL) ? <ExternalLink href={m.webURL}>{m.title}</ExternalLink> : m.title}
                  </TableCell>
                  <TableCell>{m.models.map(modelText).join(", ")}</TableCell>
                  <TableCell className={num}>{m.reviews}</TableCell>
                  <UsageCells tokens={m.tokens} />
                </TableRow>
              ))}
            </TableBody>
          </Table>
        )}
      </UsageCard>
      <Named title="Models" first="Model" rows={view.models} provider />
      <DailyTable daily={view.daily} />
      <UsageCard label="Wasted">
        <Table className={usageTable}>
          <TableHeader className="bg-muted/40">
            <UsageHead first="Kind" />
          </TableHeader>
          <TableBody>
            <TableRow>
              <TableCell>Failed skills</TableCell>
              <UsageCells tokens={wasted.failed} />
            </TableRow>
            <TableRow>
              <TableCell>Re-runs someone asked for ({wasted.rerunCount})</TableCell>
              <UsageCells tokens={wasted.reruns} />
            </TableRow>
          </TableBody>
        </Table>
        <p className="text-muted-foreground border-t px-4 py-3 text-xs">
          A skill that failed in a re-run is in both lines, so they are not added. {reviewsText(wasted.noResult)} without a result and {wasted.superseded} superseded: their tokens are unknown.
        </p>
      </UsageCard>
      <Card role="region" aria-label="Cost per review" className="gap-0 overflow-hidden py-0">
        <CardHeader className="py-3">
          <CardTitle>Cost per review</CardTitle>
        </CardHeader>
        <div className="flex items-center gap-3 border-t px-4 py-3 text-sm">
          <div className="grow">Full review median</div>
          <div className={num}>{median(view.fullMedian)}</div>
        </div>
        <div className="flex items-center gap-3 border-t px-4 py-3 text-sm">
          <div className="grow">Incremental review median</div>
          <div className={num}>{median(view.incrementalMedian)}</div>
        </div>
        <CardHeader className="border-t py-3">
          <CardTitle>Reviews over three times their kind's median</CardTitle>
        </CardHeader>
        <div className="border-t">
          {view.outliers.length === 0 ? (
            <EmptyRow>None in this period.</EmptyRow>
          ) : (
            <Table className={usageTable}>
              <TableHeader className="bg-muted/40">
                <TableRow className="hover:bg-transparent">
                  <TableHead>Review</TableHead>
                  <TableHead>Kind</TableHead>
                  <TableHead className={num}>Tokens</TableHead>
                  <TableHead>When</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {view.outliers.map((o) => (
                  <TableRow key={o.runID}>
                    <TableCell>
                      <Link className="text-primary hover:underline" to={paths.review(instance, project, o.iid)}>
                        Change {o.iid}
                      </Link>
                    </TableCell>
                    <TableCell>{o.incremental ? "Incremental" : "Full"}</TableCell>
                    <TableCell className={num}>{count(o.total)}</TableCell>
                    <TableCell>{when(o.at)}</TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          )}
        </div>
      </Card>
    </div>
  );
}

// Named is a table of skills or models, each with its runs. Only the skills
// have failed runs: the API has no count of them for a model, so a model's
// table leaves the column out instead of showing a 0 that means nothing. Only
// the models have a provider, and one name can be there under two of them.
function Named({
  title,
  first,
  rows,
  failed = false,
  provider = false,
}: {
  title: string;
  first: string;
  rows: UsageNamedView[];
  failed?: boolean;
  provider?: boolean;
}) {
  return (
    <UsageCard label={title}>
      {rows.length === 0 ? (
        <EmptyRow>No usage in this period.</EmptyRow>
      ) : (
        <Table className={usageTable}>
          <TableHeader className="bg-muted/40">
            <UsageHead first={first}>
              {provider && <TableHead>Provider</TableHead>}
              <TableHead className={num}>Runs</TableHead>
              {failed && <TableHead className={num}>Failed</TableHead>}
            </UsageHead>
          </TableHeader>
          <TableBody>
            {rows.map((r) => (
              <TableRow key={`${r.provider}\0${r.name}`}>
                <TableCell>{r.name}</TableCell>
                {provider && <TableCell>{r.provider === "" ? dash : r.provider}</TableCell>}
                <TableCell className={num}>{r.runs}</TableCell>
                {failed && <TableCell className={num}>{r.failed}</TableCell>}
                <UsageCells tokens={r.tokens} />
              </TableRow>
            ))}
          </TableBody>
        </Table>
      )}
    </UsageCard>
  );
}
