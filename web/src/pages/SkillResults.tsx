import { useQuery, type UseQueryResult } from "@tanstack/react-query";
import { useState } from "react";

import { api, paths, type SkillResultView, type SkillTokensView } from "../api";
import { useMe } from "../layout/me";
import { ErrorBox, Loading, Pill, count } from "../ui";

const periods: (30 | 90)[] = [30, 90];
const dayMs = 24 * 3600 * 1000;

// editedText says a skill changed during the window, in whole days.
function editedText(editedAt: string, now = Date.now()): string {
  const days = Math.max(0, Math.floor((now - new Date(editedAt).getTime()) / dayMs));
  const when = days === 0 ? "today" : days === 1 ? "1 day ago" : `${days} days ago`;
  return `Edited ${when}: numbers include the old version`;
}

// SkillResults is what became of the findings of each skill once their change
// was merged or closed, so a maintainer can see which skills to fix or drop.
// Addressed is Fixed over Raised and shows from 10 raised. Everything is text;
// the skill names come from the repository. An admin also sees what each
// skill cost on the same changes.
export default function SkillResults({ instance, project }: { instance: string; project: string }) {
  const admin = useMe().admin;
  const [days, setDays] = useState<30 | 90>(30);
  const list = useQuery({
    queryKey: ["skill-results", instance, project, days],
    queryFn: () => api.get<SkillResultView[]>(paths.apiRepoSkillResults(instance, project, days)),
  });
  // The server answers 404 to anyone but an admin, so no one else asks.
  const tokens = useQuery({
    queryKey: ["skill-tokens", instance, project, days],
    queryFn: () => api.get<SkillTokensView[]>(paths.apiRepoSkillTokens(instance, project, days)),
    enabled: admin,
  });

  return (
    <>
      <div className="top">
        <p className="sub">
          What became of each skill's findings when their change was merged or closed, in the last {days} days. Addressed is Fixed out of
          Raised, and shows from 10 raised. Raised, Fixed, Open and Went away count merged changes opened by people; Dismissed also counts
          closed ones.
        </p>
        <div className="actions-row">
          <select aria-label="Period" className="btn" value={days} onChange={(e) => setDays(Number(e.target.value) as 30 | 90)}>
            {periods.map((d) => (
              <option key={d} value={d}>
                Last {d} days
              </option>
            ))}
          </select>
        </div>
      </div>
      {list.isPending && <Loading />}
      {list.isError && <ErrorBox error={list.error} />}
      {list.isSuccess &&
        (list.data.length === 0 ? (
          <section className="card" aria-label="Skill results">
            <h2>Skill results</h2>
            <div className="row empty-row">No results for this period. Numbers start with changes merged after the release that added this tab.</div>
          </section>
        ) : (
          <section className="card scroll" aria-label="Skill results">
            <table className="table">
              <thead>
                <tr>
                  <th>Skill</th>
                  <th className="num">Raised</th>
                  <th className="num">Fixed</th>
                  <th className="num">Dismissed</th>
                  <th className="num">Open at merge</th>
                  <th className="num">Went away</th>
                  <th className="num">Addressed</th>
                </tr>
              </thead>
              <tbody>
                {list.data.map((r) => (
                  <tr key={`${r.skill}/${r.severity}`}>
                    <td>
                      <div className="t">{r.skill}</div>
                      <div>
                        <Pill tone={r.severity === "blocking" ? "bad" : "grey"}>{r.severity}</Pill>
                      </div>
                      {r.editedAt && <div className="meta">{editedText(r.editedAt)}</div>}
                    </td>
                    <td className="num">{r.raised}</td>
                    <td className="num">{r.fixed}</td>
                    <td className="num">{r.dismissed}</td>
                    <td className="num">{r.open}</td>
                    <td className="num">{r.wentAway}</td>
                    <td className="num">{r.addressed === null ? <span className="meta">Under 10 raised</span> : <b>{`${r.addressed}%`}</b>}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </section>
        ))}
      {admin && list.isSuccess && <SkillCost tokens={tokens} results={list.data} />}
    </>
  );
}

// SkillCost is the tokens each skill used on the changes the table counts,
// with what a fixed finding cost: tokens in and out over the findings fixed,
// summed over the skill's rows (blocking and suggestion). A skill with no
// fixed finding has no such number.
function SkillCost({ tokens, results }: { tokens: UseQueryResult<SkillTokensView[]>; results: SkillResultView[] }) {
  const fixed = new Map<string, number>();
  for (const r of results) fixed.set(r.skill, (fixed.get(r.skill) ?? 0) + r.fixed);
  return (
    <section className="card scroll" aria-label="Skill cost on these changes">
      <h2>Skill cost on these changes</h2>
      {tokens.isPending && <Loading />}
      {tokens.isError && <ErrorBox error={tokens.error} />}
      {tokens.isSuccess &&
        (tokens.data.length === 0 ? (
          <div className="row empty-row">No usage recorded for these changes.</div>
        ) : (
          <table className="table">
            <thead>
              <tr>
                <th>Skill</th>
                <th className="num">Tokens in</th>
                <th className="num">Tokens out</th>
                <th className="num">Fixed</th>
                <th className="num">Tokens per fixed finding</th>
              </tr>
            </thead>
            <tbody>
              {tokens.data.map((t) => {
                const n = fixed.get(t.skill) ?? 0;
                return (
                  <tr key={t.skill}>
                    <td>{t.skill}</td>
                    <td className="num">{count(t.tokens.in)}</td>
                    <td className="num">{count(t.tokens.out)}</td>
                    <td className="num">{n}</td>
                    <td className="num">{n > 0 ? count(Math.round((t.tokens.in + t.tokens.out) / n)) : "—"}</td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        ))}
    </section>
  );
}
