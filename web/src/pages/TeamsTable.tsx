import { Link, useSearchParams } from "react-router";

import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";

import { paths } from "../api";
import { SearchField } from "../ui";
import { SortHeader, compareRows, useTableSort } from "../TableSort";
import { useSearchText } from "../useSearchText";

// One row of the table: a team and what it covers.
export type TeamRow = {
  id: number;
  name: string;
  // The user's role on the team. Left out, the Your role column is not shown.
  role?: string;
  repoCount: number;
  instanceCount: number;
  ownedCount: number;
  memberCount: number;
};

// The columns the table sorts by, and the ones that start with the biggest first.
const SORT_KEYS = ["team", "repos", "owned", "members"] as const;
type SortKey = (typeof SORT_KEYS)[number];
const NUMERIC_KEYS: readonly SortKey[] = ["repos", "owned", "members"];

// sortValue is what a row is ordered by under a column.
function sortValue(r: TeamRow, key: SortKey): string | number {
  if (key === "repos") return r.repoCount;
  if (key === "owned") return r.ownedCount;
  if (key === "members") return r.memberCount;
  return r.name;
}

// num is a number column: right-aligned, with digits of one width.
const num = "text-right tabular-nums";

// sortHeads gives the plain th cells of SortHeader the TableHead look, and
// right-aligns the number columns (SortHeader marks them "num").
const sortHeads =
  "bg-muted/40 [&_th]:text-muted-foreground [&_th]:h-10 [&_th]:px-3 [&_th]:text-left [&_th]:align-middle [&_th]:font-medium [&_th]:whitespace-nowrap [&_th.num]:text-right";

// TeamsTable lists teams as a table that can be searched by name and sorted.
// Each name links to the team's page.
export default function TeamsTable({ rows, showRole }: { rows: TeamRow[]; showRole: boolean }) {
  const [typed, setTyped, q] = useSearchText();
  const [sort, onSort] = useTableSort<SortKey>(SORT_KEYS, "team", NUMERIC_KEYS);
  const [, setParams] = useSearchParams();
  const needle = q.toLowerCase();
  const clear = () => {
    setTyped("");
    setParams(
      (old) => {
        const next = new URLSearchParams(old);
        next.delete("q");
        return next;
      },
      { replace: true },
    );
  };
  const shown = rows
    .filter((r) => needle === "" || r.name.toLowerCase().includes(needle))
    .sort((a, b) => compareRows(a, b, (r) => sortValue(r, sort.key), sort.desc, (r) => r.name));
  return (
    <Card className="gap-0 overflow-hidden py-0">
      <div className="flex flex-wrap items-center gap-3 border-b px-4 py-3">
        <SearchField label="Search teams" value={typed} onChange={setTyped} />
        {needle !== "" && (
          <span className="text-muted-foreground text-xs">
            {shown.length} of {rows.length} shown
          </span>
        )}
      </div>
      {shown.length === 0 ? (
        <div className="text-muted-foreground flex items-center gap-2 px-4 py-6 text-sm">
          No team matches.{" "}
          <Button type="button" variant="outline" size="sm" onClick={clear}>
            Clear
          </Button>
        </div>
      ) : (
        <Table>
          {/* SortHeader draws plain th cells: they take the TableHead look from here. */}
          <TableHeader className={sortHeads}>
            <TableRow className="hover:bg-transparent">
              <SortHeader label="Team" sortKey="team" sort={sort} onSort={onSort} />
              {showRole && <TableHead>Your role</TableHead>}
              <SortHeader label="Repositories" sortKey="repos" sort={sort} onSort={onSort} num />
              <SortHeader label="Owner of" sortKey="owned" sort={sort} onSort={onSort} num />
              <SortHeader label="Members" sortKey="members" sort={sort} onSort={onSort} num />
            </TableRow>
          </TableHeader>
          <TableBody>
            {shown.map((r) => (
              <TableRow key={r.id}>
                <TableCell>
                  <Link className="text-foreground font-medium hover:underline" to={paths.team(r.id)}>
                    {r.name}
                  </Link>
                </TableCell>
                {showRole && <TableCell>{r.role}</TableCell>}
                <TableCell className={num}>
                  {r.repoCount}
                  {r.instanceCount > 0 && (
                    <div className="text-muted-foreground text-xs">
                      +{r.instanceCount} {r.instanceCount === 1 ? "instance" : "instances"}
                    </div>
                  )}
                </TableCell>
                <TableCell className={num}>{r.ownedCount}</TableCell>
                <TableCell className={num}>{r.memberCount}</TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      )}
    </Card>
  );
}
