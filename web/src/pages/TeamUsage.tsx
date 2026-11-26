import { useQuery } from "@tanstack/react-query";
import { useState } from "react";
import { Link } from "react-router";

import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";

import { api, paths, type TeamUsageView, type UsageNamedView } from "../api";
import { ErrorBox, Loading } from "../ui";
import {
  DailyTable,
  dash,
  EmptyRow,
  num,
  PeriodSelect,
  UsageCard,
  UsageCells,
  UsageHead,
  UsageStats,
  usageIntro,
  usageTable,
} from "./usageParts";

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
      <div className="flex flex-wrap items-center justify-between gap-4">
        <div className="text-muted-foreground text-sm">Tokens of the reviews in the repositories this team owns, in the last {days} days</div>
        <div className="flex items-center gap-2">
          <PeriodSelect days={days} onChange={setDays} />
        </div>
      </div>
      <p className="text-muted-foreground text-sm">
        Days are cut at midnight UTC. {usageIntro} A review counts on the team that owned the repository when it ran.
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
      {view.partialRange && <p className="text-muted-foreground text-sm">Counts reviews since v0.19.0</p>}
      <UsageStats tokens={tokens} reviews={view.reviews} />
      <UsageCard label="Repositories">
        {view.repositories.length === 0 ? (
          <EmptyRow>No usage in this period.</EmptyRow>
        ) : (
          <Table className={usageTable}>
            <TableHeader className="bg-muted/40">
              <UsageHead first="Repository">
                <TableHead className={num}>Reviews</TableHead>
              </UsageHead>
            </TableHeader>
            <TableBody>
              {view.repositories.map((r) => (
                <TableRow key={`${r.instance}/${r.project}`}>
                  <TableCell>
                    <Link className="text-foreground font-medium hover:underline" to={paths.repo(r.instance, r.project)}>
                      {`${r.instance}/${r.project}`}
                    </Link>
                  </TableCell>
                  <TableCell className={num}>{r.reviews}</TableCell>
                  <UsageCells tokens={r.tokens} />
                </TableRow>
              ))}
            </TableBody>
          </Table>
        )}
      </UsageCard>
      <Models rows={view.models} />
      <DailyTable daily={view.daily} />
    </>
  );
}

// Models is the team's tokens per model: one name can be there under two
// providers.
function Models({ rows }: { rows: UsageNamedView[] }) {
  return (
    <UsageCard label="Models">
      {rows.length === 0 ? (
        <EmptyRow>No usage in this period.</EmptyRow>
      ) : (
        <Table className={usageTable}>
          <TableHeader className="bg-muted/40">
            <UsageHead first="Model">
              <TableHead>Provider</TableHead>
              <TableHead className={num}>Runs</TableHead>
            </UsageHead>
          </TableHeader>
          <TableBody>
            {rows.map((r) => (
              <TableRow key={`${r.provider}\0${r.name}`}>
                <TableCell>{r.name}</TableCell>
                <TableCell>{r.provider === "" ? dash : r.provider}</TableCell>
                <TableCell className={num}>{r.runs}</TableCell>
                <UsageCells tokens={r.tokens} />
              </TableRow>
            ))}
          </TableBody>
        </Table>
      )}
    </UsageCard>
  );
}
