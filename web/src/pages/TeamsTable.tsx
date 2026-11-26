import { Link, useSearchParams } from "react-router";

import { paths } from "../api";
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
    <div className="card scroll">
      <div className="pad search">
        <input
          type="search"
          aria-label="Search teams"
          placeholder="Search by name"
          maxLength={200}
          value={typed}
          onChange={(e) => setTyped(e.target.value)}
        />
        {needle !== "" && (
          <span className="meta">
            {shown.length} of {rows.length} shown
          </span>
        )}
      </div>
      {shown.length === 0 ? (
        <div className="pad empty">
          No team matches.{" "}
          <button type="button" className="btn" onClick={clear}>
            Clear
          </button>
        </div>
      ) : (
        <table className="table">
          <thead>
            <tr>
              <SortHeader label="Team" sortKey="team" sort={sort} onSort={onSort} />
              {showRole && <th>Your role</th>}
              <SortHeader label="Repositories" sortKey="repos" sort={sort} onSort={onSort} num />
              <SortHeader label="Owner of" sortKey="owned" sort={sort} onSort={onSort} num />
              <SortHeader label="Members" sortKey="members" sort={sort} onSort={onSort} num />
            </tr>
          </thead>
          <tbody>
            {shown.map((r) => (
              <tr key={r.id}>
                <td>
                  <Link to={paths.team(r.id)}>{r.name}</Link>
                </td>
                {showRole && <td>{r.role}</td>}
                <td className="num">
                  {r.repoCount}
                  {r.instanceCount > 0 && (
                    <div className="meta">
                      +{r.instanceCount} {r.instanceCount === 1 ? "instance" : "instances"}
                    </div>
                  )}
                </td>
                <td className="num">{r.ownedCount}</td>
                <td className="num">{r.memberCount}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </div>
  );
}
