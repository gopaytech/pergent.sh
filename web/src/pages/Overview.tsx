import { useQuery } from "@tanstack/react-query";
import { useState } from "react";
import { Link } from "react-router";

import { api, type OverviewView, type SetupView } from "../api";
import { useMe } from "../layout/me";
import { ErrorBox, Loading } from "../ui";
import ReviewRows from "./ReviewRows";

const periods = [7, 30, 90];

// Overview is the landing page: the numbers of the last days and the recent
// reviews. A repository's setup problems are under its Setup checks tab.
export default function Overview() {
  const me = useMe();
  const [days, setDays] = useState(7);
  const data = useQuery({
    queryKey: ["overview", days],
    queryFn: () => api.get<OverviewView>(`/api/overview?days=${days}`),
  });
  return (
    <>
      <div className="top">
        <div>
          <h1>Overview</h1>
          <div className="sub">Last {days} days across the repositories you can see</div>
        </div>
        <div className="actions-row">
          <select aria-label="Period" className="btn" value={days} onChange={(e) => setDays(Number(e.target.value))}>
            {periods.map((d) => (
              <option key={d} value={d}>
                Last {d} days
              </option>
            ))}
          </select>
          {me.admin && (
            <Link className="btn primary" to="/repos?add=1">
              + Add repository
            </Link>
          )}
        </div>
      </div>
      {data.isPending && <Loading />}
      {data.isError && <ErrorBox error={data.error} />}
      {data.isSuccess && <Body view={data.data} days={days} />}
    </>
  );
}

function Body({ view, days }: { view: OverviewView; days: number }) {
  const { stats } = view;
  const pct = stats.reviews > 0 ? Math.round((stats.approved / stats.reviews) * 100) : 0;
  // Each number opens the reviews behind it, over the same days.
  const reviews = (outcome?: string) => `/reviews?${outcome ? `outcome=${outcome}&` : ""}days=${days}`;
  return (
    <>
      {view.setup && <Checklist setup={view.setup} />}
      <section className="stats" aria-label="Numbers">
        <Stat k="Reviews" v={stats.reviews} d="not counting superseded or skipped runs" to={reviews()} />
        <Stat k="Approved by pergent" v={stats.approved} d={`${pct}% of reviews`} to={reviews("approved")} />
        <Stat k="Blocking findings" v={stats.blocking} d="reviews with blocking issues" to={reviews("blocking")} />
        <Stat
          k="Failed reviews"
          v={stats.failed}
          d={stats.failed > 0 ? "open them to see why" : "none failed"}
          bad={stats.failed > 0}
          to={stats.failed > 0 ? reviews("failed") : undefined}
        />
      </section>
      <section className="card" aria-label="Recent reviews">
        <h2>
          Recent reviews <Link to={reviews()}>View all →</Link>
        </h2>
        <ReviewRows items={view.recent} empty="No reviews yet." />
      </section>
    </>
  );
}

// The setup steps of a new install, in order, each with the page that does it.
const setupSteps: [keyof SetupView, string, string][] = [
  ["llm", "Set the model and its key", "/admin/settings"],
  ["instance", "Connect a GitLab or GitHub instance", "/admin/integrations"],
  ["skill", "Add a skill to the library", "/skills"],
  ["repository", "Switch on a repository", "/repos?add=1"],
];

// Checklist shows an admin what is left to set up. The overview sends the
// setup only while something is left, so it goes away once all four are done.
function Checklist({ setup }: { setup: SetupView }) {
  return (
    <section className="card" aria-label="Setup checklist">
      <h2>Get started</h2>
      <ul className="checklist">
        {setupSteps.map(([key, label, to]) => (
          <li key={key} className={setup[key] ? "done" : undefined}>
            <span aria-label={setup[key] ? "done" : "to do"} role="img" className="tick">
              {setup[key] ? "✓" : "○"}
            </span>
            <Link to={to}>{label}</Link>
          </li>
        ))}
      </ul>
    </section>
  );
}

// Stat is one number; with to, the whole card opens the reviews behind it.
function Stat({ k, v, d, bad, to }: { k: string; v: number; d: string; bad?: boolean; to?: string }) {
  const body = (
    <>
      <div className="k">{k}</div>
      <div className="v">{v}</div>
      <div className={bad ? "d down" : "d meta"}>{d}</div>
    </>
  );
  return to ? (
    <Link className="stat" to={to} aria-label={`${k}: ${v}. Open these reviews`}>
      {body}
    </Link>
  ) : (
    <div className="stat">{body}</div>
  );
}
