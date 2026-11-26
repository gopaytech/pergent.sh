import { useQuery } from "@tanstack/react-query";
import { Link } from "react-router";

import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";

import { api, paths, type UsageClientView, type UsageOverviewView } from "../api";
import { ErrorBox, Loading, PageHeader, Pill, count } from "../ui";
import {
  CsvLink,
  DailyTable,
  EmptyRow,
  PeriodSelect,
  periods,
  num,
  UsageCard,
  UsageCells,
  UsageHead,
  UsageNotes,
  UsageStats,
  usageTable,
} from "./usageParts";
import { useUrlParam } from "../urlState";

// Usage is where the model tokens went across every repository and API client,
// for admins: the totals, each repository with what global and team keys
// paid, each client with its tenants and each day. The numbers are text only.
export default function Usage() {
  const [days, setDays] = useUrlParam<number>("days", 30, periods);
  const data = useQuery({
    queryKey: ["usage", days],
    queryFn: () => api.get<UsageOverviewView>(paths.apiUsage(days)),
  });
  return (
    <>
      <PageHeader
        title="Usage"
        description={`Where model tokens went in the last ${days} days, across every repository and API client`}
        actions={
          <>
            <PeriodSelect days={days} onChange={setDays} />
            <CsvLink days={days} />
          </>
        }
      />
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
    <div className="flex flex-col gap-6">
      <UsageStats tokens={tokens} reviews={view.reviews} />
      <UsageCard label="Repositories">
        {view.repositories.length === 0 ? (
          <EmptyRow>No usage in this period.</EmptyRow>
        ) : (
          <Table className={usageTable}>
            <TableHeader className="bg-muted/40">
              <UsageHead
                first="Repository"
                after={
                  <>
                    <TableHead className={num}>On global keys</TableHead>
                    <TableHead className={num}>On team keys</TableHead>
                  </>
                }
              >
                <TableHead className={num}>Reviews</TableHead>
              </UsageHead>
            </TableHeader>
            <TableBody>
              {view.repositories.map((r) => (
                <TableRow key={`${r.instance}/${r.project}`}>
                  <TableCell>
                    {r.switchedOn || r.paused ? (
                      <span className="inline-flex items-center gap-2">
                        <Link className="text-primary hover:underline" to={paths.usageRepo(r.instance, r.project)}>{`${r.instance}/${r.project}`}</Link>
                        {r.paused && <Pill tone="grey">paused</Pill>}
                      </span>
                    ) : (
                      // A repository removed has no page: the API answers 404.
                      <>
                        <span className="inline-flex items-center gap-2">
                          {`${r.instance}/${r.project}`}
                          <Pill tone="grey">removed</Pill>
                        </span>
                        <div>
                          <a className="text-primary text-xs hover:underline" href={paths.apiUsageCSV(days, r.instance, r.project)} download>
                            CSV
                          </a>
                        </div>
                      </>
                    )}
                  </TableCell>
                  <TableCell className={num}>{r.reviews}</TableCell>
                  <UsageCells tokens={r.tokens} />
                  <TableCell className={num}>{count(r.globalTokens)}</TableCell>
                  <TableCell className={num}>{count(r.teamTokens)}</TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        )}
      </UsageCard>
      <UsageCard label="API clients">
        {view.clients.length === 0 ? (
          <EmptyRow>No usage in this period.</EmptyRow>
        ) : (
          <Table className={usageTable}>
            <TableHeader className="bg-muted/40">
              <UsageHead first="Client and tenants">
                <TableHead className={num}>Reviews</TableHead>
              </UsageHead>
            </TableHeader>
            <TableBody>
              {view.clients.map((c) => (
                <ClientRows key={c.name} client={c} />
              ))}
            </TableBody>
          </Table>
        )}
      </UsageCard>
      <DailyTable daily={view.daily} />
    </div>
  );
}

// ClientRows is a client's row and one row for each of its tenants under it.
// A tenant is whatever the caller sent: it is shown as text, "" as no tenant.
function ClientRows({ client }: { client: UsageClientView }) {
  return (
    <>
      <TableRow>
        <TableCell>
          <Link className="text-primary hover:underline" to={paths.clientPage(client.name)}>
            {client.name}
          </Link>
        </TableCell>
        <TableCell className={num}>{client.reviews}</TableCell>
        <UsageCells tokens={client.tokens} />
      </TableRow>
      {/* A tenant is muted and set in under its client. */}
      {client.tenants.map((t) => (
        <TableRow key={t.tenant} className="text-muted-foreground">
          <TableCell>
            <span className="pl-4">{t.tenant === "" ? "(no tenant)" : t.tenant}</span>
          </TableCell>
          <TableCell className={num}>{t.reviews}</TableCell>
          <UsageCells tokens={t.tokens} />
        </TableRow>
      ))}
      {client.moreTenants > 0 && (
        <TableRow className="text-muted-foreground">
          <TableCell colSpan={6}>
            <span className="pl-4">{`and ${client.moreTenants} more ${client.moreTenants === 1 ? "tenant" : "tenants"}`}</span>
          </TableCell>
        </TableRow>
      )}
    </>
  );
}
