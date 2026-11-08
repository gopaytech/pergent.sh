import { useQuery } from "@tanstack/react-query";
import { useState } from "react";
import { Link } from "react-router";

import { api, paths, type UsageOverviewView } from "../api";
import { ErrorBox, Loading, Pill, count } from "../ui";
import { apartText, CsvLink, DailyTable, EmptyRow, PeriodSelect, total, UsageCells, UsageHead, UsageNotes } from "./usageParts";

// Usage is where the model tokens went across every repository, for admins:
// the totals, each day and each repository. The numbers are text only.
export default function Usage() {
  const [days, setDays] = useState(30);
  const data = useQuery({
    queryKey: ["usage", days],
    queryFn: () => api.get<UsageOverviewView>(paths.apiUsage(days)),
  });
  return (
    <>
      <div className="top">
        <div>
          <h1>Usage</h1>
          <div className="sub">Where model tokens went in the last {days} days, across every repository</div>
        </div>
        <div className="actions-row">
          <PeriodSelect days={days} onChange={setDays} />
          <CsvLink days={days} />
        </div>
      </div>
      <UsageNotes />
      {data.isPending && <Loading />}
      {data.isError && <ErrorBox error={data.error} />}
      {data.isSuccess && <Body view={data.data} days={days} />}
    </>
  );
}

function Body({ view, days }: { view: UsageOverviewView; days: number }) {
  const { tokens } = view;
  return (
    <>
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
                    {r.switchedOn ? (
                      <Link to={paths.usageRepo(r.instance, r.project)}>{`${r.instance}/${r.project}`}</Link>
                    ) : (
                      // A repository switched off has no page: the API answers 404.
                      <>
                        {`${r.instance}/${r.project}`}
                        <Pill tone="grey">switched off</Pill>
                        <div>
                          <a href={paths.apiUsageCSV(days, r.instance, r.project)} download>
                            CSV
                          </a>
                        </div>
                      </>
                    )}
                  </td>
                  <td className="num">{r.reviews}</td>
                  <UsageCells tokens={r.tokens} />
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </section>
      <DailyTable daily={view.daily} />
    </>
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
