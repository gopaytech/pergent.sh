import { useQuery } from "@tanstack/react-query";
import { useMemo } from "react";
import { useSearchParams } from "react-router";

import { NativeSelect } from "@/components/ui/native-select";

import { api, query, type RepositoryView } from "../api";
import { PageHeader, outcomeOptions } from "../ui";
import ReviewList from "./ReviewList";

const times: [number, string][] = [
  [7, "Last 7 days"],
  [30, "Last 30 days"],
  [90, "Last 90 days"],
  [365, "Last year"],
];
const defaultDays = 90;

// selectWidth is the width of each select in the filter row: one line on a wide
// screen, the whole width on a phone.
const selectWidth = "sm:w-52";

// Reviews lists every merge request or pull request reviewed, filtered by
// repository, result and time; the filters live in the address.
export default function Reviews() {
  const [params, setParams] = useSearchParams();
  const project = params.get("project") ?? "";
  const outcome = params.get("outcome") ?? "";
  const daysParam = Number(params.get("days"));
  const days = times.some(([d]) => d === daysParam) ? daysParam : defaultDays;
  const repos = useQuery({ queryKey: ["repositories"], queryFn: () => api.get<RepositoryView[]>("/api/repositories") });

  const all = useMemo(() => repos.data ?? [], [repos.data]);
  // The teams that own something in the list, by name. A ?team= that is not
  // one of them, and not "none", counts as All teams.
  const teams = useMemo(() => {
    const byId = new Map<number, string>();
    for (const r of all) if (r.ownerTeam) byId.set(r.ownerTeam.id, r.ownerTeam.name);
    return [...byId].sort((a, b) => a[1].localeCompare(b[1]));
  }, [all]);
  const teamParam = params.get("team") ?? "";
  const team = teamParam === "none" || teams.some(([id]) => String(id) === teamParam) ? teamParam : "";
  const inTeam = (r: RepositoryView, t: string) => t === "" || (t === "none" ? r.ownerTeam === null : String(r.ownerTeam?.id) === t);

  const set = (next: { project?: string; outcome?: string; days?: number; team?: string }) => {
    const v = { project, outcome, days, team, ...next };
    // A repository the picked team does not own is dropped with the team change.
    if (next.team !== undefined && !all.some((r) => r.project === v.project && inTeam(r, v.team))) v.project = "";
    setParams(
      new URLSearchParams(
        query([
          ["project", v.project],
          ["outcome", v.outcome],
          ["days", v.days === defaultDays ? undefined : v.days],
          ["team", v.team],
        ]).slice(1),
      ),
    );
  };

  const projects = all.filter((r) => inTeam(r, team)).map((r) => r.project);
  if (project && !projects.includes(project)) projects.push(project);

  return (
    <>
      <PageHeader title="Reviews" description="Every merge request and pull request pergent reviewed, newest first" />
      <div className="mb-4 flex flex-wrap items-center gap-2">
        {!repos.isPending && (
          <NativeSelect className={selectWidth} aria-label="Team" value={team} onChange={(e) => set({ team: e.target.value })}>
            <option value="">All teams</option>
            <option value="none">No owner</option>
            {teams.map(([id, name]) => (
              <option key={id} value={String(id)}>
                {name}
              </option>
            ))}
          </NativeSelect>
        )}
        {!repos.isPending && (
          <NativeSelect className={selectWidth} aria-label="Repository" value={project} onChange={(e) => set({ project: e.target.value })}>
            <option value="">All repositories</option>
            {projects.map((p) => (
              <option key={p} value={p}>
                {p}
              </option>
            ))}
          </NativeSelect>
        )}
        <NativeSelect className={selectWidth} aria-label="Result" value={outcome} onChange={(e) => set({ outcome: e.target.value })}>
          <option value="">Any result</option>
          {outcomeOptions.map(([value, label]) => (
            <option key={value} value={value}>
              {label}
            </option>
          ))}
        </NativeSelect>
        <NativeSelect className={selectWidth} aria-label="Time" value={days} onChange={(e) => set({ days: Number(e.target.value) })}>
          {times.map(([d, label]) => (
            <option key={d} value={d}>
              {label}
            </option>
          ))}
        </NativeSelect>
      </div>
      {/* A ?team= is checked against the list, so wait for it. */}
      {!(repos.isPending && teamParam) && (
        <ReviewList
          filter={{ project: project || undefined, outcome: outcome || undefined, days, team: team || undefined }}
          label="Review list"
          clearTo={project || outcome || team || days !== defaultDays ? "/reviews" : undefined}
        />
      )}
    </>
  );
}
