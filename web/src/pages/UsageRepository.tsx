import { useQuery } from "@tanstack/react-query";
import { useState } from "react";
import { Link, useParams } from "react-router";

import { api, isStatus, paths, safeHref, type UsageNamedView, type UsageRepositoryView } from "../api";
import { ErrorBox, ExternalLink, Loading, NotFound, count } from "../ui";
import { CsvLink, DailyTable, dash, EmptyRow, PeriodSelect, UsageCells, UsageHead, UsageNotes, reviewsText } from "./usageParts";

// when is an RFC 3339 time as "2026-10-30 14:02 UTC" (the API sends UTC).
const when = (at: string) => `${at.slice(0, 16).replace("T", " ")} UTC`;

// median is a median of tokens, or a dash when there was no review of that kind.
const median = (m: number | null) => (m === null ? dash : count(Math.round(m)));

// modelText is a model as "provider / model", the name alone when the
// reviews recorded no provider.
const modelText = (m: { provider: string; model: string }) => (m.provider === "" ? m.model : `${m.provider} / ${m.model}`);

// UsageRepository is one repository's usage, for admins: per skill, merge
// request, model and day, what was wasted and what a review costs. Titles,
// skill and model names are text only.
export default function UsageRepository() {
  const params = useParams();
  const instance = params.instance ?? "";
  const project = params["*"] ?? "";
  const [days, setDays] = useState(30);
  const data = useQuery({
    queryKey: ["usage", instance, project, days],
    queryFn: () => api.get<UsageRepositoryView>(paths.apiRepoUsage(instance, project, days)),
    enabled: project !== "",
  });
  // A repository that is not switched on has no page: the API answers 404.
  if (project === "" || isStatus(data.error, 404)) return <NotFound />;
  return (
    <>
      <div className="crumbs">
        <Link to="/admin/usage">Usage</Link> / {project}
      </div>
      <div className="top">
        <div>
          <h1>{project}</h1>
          <div className="sub">
            {instance} · the last {days} days
          </div>
        </div>
        <div className="actions-row">
          <PeriodSelect days={days} onChange={setDays} />
          <CsvLink days={days} instance={instance} project={project} />
        </div>
      </div>
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
    <>
      <Named title="Skills" first="Skill" rows={view.skills} failed />
      <section className="card scroll" aria-label="Merge requests">
        <h2>Merge requests</h2>
        {view.mergeRequests.length === 0 ? (
          <EmptyRow>No usage in this period.</EmptyRow>
        ) : (
          <table className="table">
            <thead>
              <UsageHead first="Merge request">
                <th>Models</th>
                <th className="num">Reviews</th>
              </UsageHead>
            </thead>
            <tbody>
              {view.mergeRequests.map((m) => (
                <tr key={m.iid}>
                  <td>
                    <Link to={paths.review(instance, project, m.iid)}>{m.iid}</Link>{" "}
                    {safeHref(m.webURL) ? <ExternalLink href={m.webURL}>{m.title}</ExternalLink> : m.title}
                  </td>
                  <td>{m.models.map(modelText).join(", ")}</td>
                  <td className="num">{m.reviews}</td>
                  <UsageCells tokens={m.tokens} />
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </section>
      <Named title="Models" first="Model" rows={view.models} provider />
      <DailyTable daily={view.daily} />
      <section className="card scroll" aria-label="Wasted">
        <h2>Wasted</h2>
        <table className="table">
          <thead>
            <UsageHead first="Kind" />
          </thead>
          <tbody>
            <tr>
              <td>Failed skills</td>
              <UsageCells tokens={wasted.failed} />
            </tr>
            <tr>
              <td>Re-runs someone asked for ({wasted.rerunCount})</td>
              <UsageCells tokens={wasted.reruns} />
            </tr>
          </tbody>
        </table>
        <p className="meta pad">
          A skill that failed in a re-run is in both lines, so they are not added. {reviewsText(wasted.noResult)} without a result and {wasted.superseded} superseded: their tokens are unknown.
        </p>
      </section>
      <section className="card scroll" aria-label="Cost per review">
        <h2>Cost per review</h2>
        <div className="row">
          <div className="grow">Full review median</div>
          <div className="num">{median(view.fullMedian)}</div>
        </div>
        <div className="row">
          <div className="grow">Incremental review median</div>
          <div className="num">{median(view.incrementalMedian)}</div>
        </div>
        <h2>Reviews over three times their kind's median</h2>
        {view.outliers.length === 0 ? (
          <EmptyRow>None in this period.</EmptyRow>
        ) : (
          <table className="table">
            <thead>
              <tr>
                <th>Review</th>
                <th>Kind</th>
                <th className="num">Tokens</th>
                <th>When</th>
              </tr>
            </thead>
            <tbody>
              {view.outliers.map((o) => (
                <tr key={o.runID}>
                  <td>
                    <Link to={paths.review(instance, project, o.iid)}>Change {o.iid}</Link>
                  </td>
                  <td>{o.incremental ? "Incremental" : "Full"}</td>
                  <td className="num">{count(o.total)}</td>
                  <td>{when(o.at)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </section>
    </>
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
    <section className="card scroll" aria-label={title}>
      <h2>{title}</h2>
      {rows.length === 0 ? (
        <EmptyRow>No usage in this period.</EmptyRow>
      ) : (
        <table className="table">
          <thead>
            <UsageHead first={first}>
              {provider && <th>Provider</th>}
              <th className="num">Runs</th>
              {failed && <th className="num">Failed</th>}
            </UsageHead>
          </thead>
          <tbody>
            {rows.map((r) => (
              <tr key={`${r.provider}\0${r.name}`}>
                <td>{r.name}</td>
                {provider && <td>{r.provider === "" ? dash : r.provider}</td>}
                <td className="num">{r.runs}</td>
                {failed && <td className="num">{r.failed}</td>}
                <UsageCells tokens={r.tokens} />
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </section>
  );
}
