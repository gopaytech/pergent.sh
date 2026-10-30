import { useQuery } from "@tanstack/react-query";
import { useSearchParams } from "react-router";

import { api, query, type RepositoryView } from "../api";
import { outcomeOptions } from "../ui";
import ReviewList from "./ReviewList";

const times: [number, string][] = [
  [7, "Last 7 days"],
  [30, "Last 30 days"],
  [90, "Last 90 days"],
  [365, "Last year"],
];
const defaultDays = 90;

// Reviews lists every merge request or pull request reviewed, filtered by
// repository, result and time; the filters live in the address.
export default function Reviews() {
  const [params, setParams] = useSearchParams();
  const project = params.get("project") ?? "";
  const outcome = params.get("outcome") ?? "";
  const daysParam = Number(params.get("days"));
  const days = times.some(([d]) => d === daysParam) ? daysParam : defaultDays;
  const repos = useQuery({ queryKey: ["repositories"], queryFn: () => api.get<RepositoryView[]>("/api/repositories") });

  const set = (next: { project?: string; outcome?: string; days?: number }) => {
    const v = { project, outcome, days, ...next };
    setParams(
      new URLSearchParams(
        query([
          ["project", v.project],
          ["outcome", v.outcome],
          ["days", v.days === defaultDays ? undefined : v.days],
        ]).slice(1),
      ),
    );
  };

  const projects = (repos.data ?? []).map((r) => r.project);
  if (project && !projects.includes(project)) projects.push(project);

  return (
    <>
      <div className="top">
        <div>
          <h1>Reviews</h1>
          <div className="sub">Every merge request and pull request pergent reviewed, newest first</div>
        </div>
      </div>
      <div className="filters">
        {!repos.isPending && (
          <select aria-label="Repository" value={project} onChange={(e) => set({ project: e.target.value })}>
            <option value="">All repositories</option>
            {projects.map((p) => (
              <option key={p} value={p}>
                {p}
              </option>
            ))}
          </select>
        )}
        <select aria-label="Result" value={outcome} onChange={(e) => set({ outcome: e.target.value })}>
          <option value="">Any result</option>
          {outcomeOptions.map(([value, label]) => (
            <option key={value} value={value}>
              {label}
            </option>
          ))}
        </select>
        <select aria-label="Time" value={days} onChange={(e) => set({ days: Number(e.target.value) })}>
          {times.map(([d, label]) => (
            <option key={d} value={d}>
              {label}
            </option>
          ))}
        </select>
      </div>
      <ReviewList filter={{ project: project || undefined, outcome: outcome || undefined, days }} label="Review list" />
    </>
  );
}
