import { useQuery } from "@tanstack/react-query";
import { useState } from "react";
import { Link } from "react-router";

import { api, paths, type UsageClientView, type UsageOverviewView } from "../api";
import { ErrorBox, Loading, Pill, count } from "../ui";
import { apartText, CsvLink, DailyTable, EmptyRow, PeriodSelect, total, UsageCells, UsageHead, UsageNotes } from "./usageParts";

// Usage is where the model tokens went across every repository and API client,
// for admins: the totals, each repository, each client with its tenants and
// each day. The numbers are text only.
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
          <div className="sub">Where model tokens went in the last {days} days, across every repository and API client</div>
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
      <section className="card scroll" aria-label="API clients">
        <h2>API clients</h2>
        {view.clients.length === 0 ? (
          <EmptyRow>No usage in this period.</EmptyRow>
        ) : (
          <table className="table">
            <thead>
              <UsageHead first="Client and tenants">
                <th className="num">Reviews</th>
              </UsageHead>
            </thead>
            <tbody>
              {view.clients.map((c) => (
                <ClientRows key={c.name} client={c} />
              ))}
            </tbody>
          </table>
        )}
      </section>
      <DailyTable daily={view.daily} />
    </>
  );
}

// ClientRows is a client's row and one row for each of its tenants under it.
// A tenant is whatever the caller sent: it is shown as text, "" as no tenant.
function ClientRows({ client }: { client: UsageClientView }) {
  return (
    <>
      <tr>
        <td>
          <Link to={paths.clientPage(client.name)}>{client.name}</Link>
        </td>
        <td className="num">{client.reviews}</td>
        <UsageCells tokens={client.tokens} />
      </tr>
      {client.tenants.map((t) => (
        <tr key={t.tenant} className="tenant-row">
          <td>{t.tenant === "" ? "(no tenant)" : t.tenant}</td>
          <td className="num">{t.reviews}</td>
          <UsageCells tokens={t.tokens} />
        </tr>
      ))}
      {client.moreTenants > 0 && (
        <tr className="tenant-row">
          <td colSpan={6}>{`and ${client.moreTenants} more ${client.moreTenants === 1 ? "tenant" : "tenants"}`}</td>
        </tr>
      )}
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
