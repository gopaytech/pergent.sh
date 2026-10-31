import { useQuery } from "@tanstack/react-query";
import { useState } from "react";
import { Link } from "react-router";

import { api, type OverviewView } from "../api";
import { useMe } from "../layout/me";
import { ErrorBox, Loading } from "../ui";
import AttentionRows from "./Attention";
import ReviewRows from "./ReviewRows";

const periods = [7, 30, 90];

// Overview is the landing page: the numbers of the last days, the recent
// reviews and the setup problems.
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
      {data.isSuccess && <Body view={data.data} platform={me.platform} />}
    </>
  );
}

function Body({ view, platform }: { view: OverviewView; platform: string }) {
  const { stats } = view;
  const pct = stats.reviews > 0 ? Math.round((stats.approved / stats.reviews) * 100) : 0;
  return (
    <>
      <section className="stats" aria-label="Numbers">
        <Stat k="Reviews" v={stats.reviews} d="not counting superseded runs" />
        <Stat k="Approved by pergent" v={stats.approved} d={`${pct}% of reviews`} />
        <Stat k="Blocking findings" v={stats.blocking} d="reviews with blocking issues" />
        <Stat k="Failed reviews" v={stats.failed} d={stats.failed > 0 ? "see the reviews for why" : "none failed"} bad={stats.failed > 0} />
      </section>
      <div className="grid">
        <section className="card" aria-label="Recent reviews">
          <h2>
            Recent reviews <Link to="/reviews">View all →</Link>
          </h2>
          <ReviewRows items={view.recent} empty="No reviews yet." />
        </section>
        <section className="card attention" aria-label="Needs attention">
          <h2>Needs attention</h2>
          <AttentionRows items={view.attention} platform={platform} />
        </section>
      </div>
    </>
  );
}

function Stat({ k, v, d, bad }: { k: string; v: number; d: string; bad?: boolean }) {
  return (
    <div className="stat">
      <div className="k">{k}</div>
      <div className="v">{v}</div>
      <div className={bad ? "d down" : "d meta"}>{d}</div>
    </div>
  );
}
