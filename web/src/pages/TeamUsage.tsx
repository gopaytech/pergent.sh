import { useQuery } from "@tanstack/react-query";
import { useState } from "react";
import { Link } from "react-router";

import { api, paths, type TeamUsageView, type UsageNamedView } from "../api";
import { ErrorBox, Loading, count } from "../ui";
import { apartText, DailyTable, dash, EmptyRow, PeriodSelect, total, UsageCells, UsageHead } from "./usageParts";

// TeamUsage is the Usage tab of a team's page, for its maintainers and
// admins: the tokens of the reviews the team owns, each day, each repository
// and each model, busiest first. The numbers are text only.
export default function TeamUsage({ team }: { team: number }) {
  const [days, setDays] = useState(30);
  const data = useQuery({
    queryKey: ["team-usage", team, days],
    queryFn: () => api.get<TeamUsageView>(paths.apiTeamUsage(team, days)),
  });
  return (
    <>
      <div className="top">
        <div className="sub">Tokens of the reviews in the repositories this team owns, in the last {days} days</div>
        <div className="actions-row">
          <PeriodSelect days={days} onChange={setDays} />
        </div>
      </div>
      <p className="sub">
        Days are cut at midnight UTC. Totals add input and output tokens only; cache and reasoning counts are shown apart and are not in them. A review counts on the team that owned the repository when it ran.
      </p>
      {data.isPending && <Loading />}
      {data.isError && <ErrorBox error={data.error} />}
      {data.isSuccess && <Body view={data.data} />}
    </>
  );
}

function Body({ view }: { view: TeamUsageView }) {
  const { tokens } = view;
  return (
    <>
      {view.partialRange && <p className="sub">Counts reviews since v0.19.0</p>}
      <section className="stats" aria-label="Totals">
        <Stat k="Tokens" v={count(total(tokens))} d="input plus output" />
        <Stat k="Input" v={count(tokens.in)} d="tokens sent to the model" />
        <Stat k="Output" v={count(tokens.out)} d="tokens the model wrote" />
        <Stat k="Reviews" v={String(view.reviews)} d="with usage recorded" />
      </section>
      <p className="sub">Reported apart, not in the totals: {apartText(tokens)}</p>
      <section className="card scroll" aria-label="Repositories">
        <h2>Repositories</h2>
        {view.repositories.length === 0 ? (
          <EmptyRow>No usage in this period.</EmptyRow>
        ) : (
          <table className="table">
            <thead>
              <UsageHead first="Repository">
                <th className="num">Reviews</th>
              </UsageHead>
            </thead>
            <tbody>
              {view.repositories.map((r) => (
                <tr key={`${r.instance}/${r.project}`}>
                  <td>
                    <Link to={paths.repo(r.instance, r.project)}>{`${r.instance}/${r.project}`}</Link>
                  </td>
                  <td className="num">{r.reviews}</td>
                  <UsageCells tokens={r.tokens} />
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </section>
      <Models rows={view.models} />
      <DailyTable daily={view.daily} />
    </>
  );
}

// Models is the team's tokens per model: one name can be there under two
// providers.
function Models({ rows }: { rows: UsageNamedView[] }) {
  return (
    <section className="card scroll" aria-label="Models">
      <h2>Models</h2>
      {rows.length === 0 ? (
        <EmptyRow>No usage in this period.</EmptyRow>
      ) : (
        <table className="table">
          <thead>
            <UsageHead first="Model">
              <th>Provider</th>
              <th className="num">Runs</th>
            </UsageHead>
          </thead>
          <tbody>
            {rows.map((r) => (
              <tr key={`${r.provider}\0${r.name}`}>
                <td>{r.name}</td>
                <td>{r.provider === "" ? dash : r.provider}</td>
                <td className="num">{r.runs}</td>
                <UsageCells tokens={r.tokens} />
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </section>
  );
}

function Stat({ k, v, d }: { k: string; v: string; d: string }) {
  return (
    <div className="stat">
      <div className="k">{k}</div>
      <div className="v">{v}</div>
      <div className="d meta">{d}</div>
    </div>
  );
}
