import type { ReactNode } from "react";

import { paths, type UsageDayView, type UsageTokensView } from "../api";
import { count } from "../ui";

export const periods = [7, 30, 90];

// dash stands for a number that doesn't exist, such as the median of no reviews.
export const dash = "—";

// total, prompt and output are the numbers the server adds up: every token
// counts the same. prompt is input, cache read and cache write; output is
// output and reasoning; total is both.
export const total = (t: UsageTokensView) => t.total;
export const prompt = (t: UsageTokensView) => t.prompt;
export const output = (t: UsageTokensView) => t.output;

// reviews says "1 review" or "3 reviews".
export const reviewsText = (n: number) => `${n} ${n === 1 ? "review" : "reviews"}`;

// skillRunsText says "1 skill run" or "3 skill runs": what unknown counts is
// the runs of a skill, so a review of two skills is two of them.
const skillRunsText = (n: number) => `${n} ${n === 1 ? "skill run" : "skill runs"}`;

// cachedPercent is the share of the prompt read from the cache, as a whole
// percent. It is a dash when there is no prompt, or when unknown skill runs
// left cache and reasoning with nothing reported (a 0% would be a guess).
export function cachedPercent(t: UsageTokensView): string {
  if (t.prompt === 0) return dash;
  if (t.unknown > 0 && t.cacheRead + t.cacheWrite + t.reasoning === 0) return dash;
  return `${Math.round((t.cacheRead / t.prompt) * 100)}%`;
}

// partsText is the 5 parts a total adds up, and how many skill runs reported
// no cache or reasoning (unknown).
export function partsText(t: UsageTokensView): string {
  const parts = [
    `input ${count(t.in)}`,
    `cache read ${count(t.cacheRead)}`,
    `cache write ${count(t.cacheWrite)}`,
    `output ${count(t.out)}`,
    `reasoning ${count(t.reasoning)}`,
  ];
  if (t.unknown > 0) parts.push(`unknown for ${skillRunsText(t.unknown)}`);
  return parts.join(" · ");
}

// PeriodSelect is the period menu of the Usage pages.
export function PeriodSelect({ days, onChange }: { days: number; onChange: (d: number) => void }) {
  return (
    <select aria-label="Period" className="btn" value={days} onChange={(e) => onChange(Number(e.target.value))}>
      {periods.map((d) => (
        <option key={d} value={d}>
          Last {d} days
        </option>
      ))}
    </select>
  );
}

// CsvLink is a plain download link: a GET on the same origin sends the
// session cookie, and it needs no CSRF token.
export function CsvLink({ days, instance, project }: { days: number; instance?: string; project?: string }) {
  return (
    <a className="btn" href={paths.apiUsageCSV(days, instance, project)} download>
      Download CSV
    </a>
  );
}

// usageIntro says what a total counts; the Usage pages and the team tab share it.
export const usageIntro = "Totals count all tokens: input, cache read, cache write, output and reasoning.";

// UsageNotes is the line both pages carry about how the numbers are made.
export function UsageNotes() {
  return (
    <p className="sub">
      Days are cut at midnight UTC. {usageIntro}
    </p>
  );
}

// UsageStats is the 4 cards on top of a Usage page, and the lines that say
// what Prompt and Output hold.
export function UsageStats({ tokens, reviews }: { tokens: UsageTokensView; reviews: number }) {
  const cached = cachedPercent(tokens);
  return (
    <>
      <section className="stats" aria-label="Totals">
        <Stat k="Tokens" v={count(total(tokens))} d="all token kinds added" />
        <Stat k="Prompt" v={count(prompt(tokens))} d={cached === dash ? "cache share unknown" : `${cached} from cache`} />
        <Stat k="Output" v={count(output(tokens))} d="incl. reasoning" />
        <Stat k="Reviews" v={String(reviews)} d="with usage recorded" />
      </section>
      <p className="sub">Prompt: input · cache read · cache write</p>
      <p className="sub">Output: output · reasoning</p>
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

// UsageHead is the column heads UsageCells fills; first is the head of the
// column before them, children the heads between, after those after them.
export function UsageHead({ first, children, after }: { first: string; children?: ReactNode; after?: ReactNode }) {
  return (
    <tr>
      <th>{first}</th>
      {children}
      <th className="num">Prompt</th>
      <th className="num">Output</th>
      <th className="num">Tokens</th>
      <th className="num">Cached</th>
      {after}
    </tr>
  );
}

// sourceText is a usage's tokens split by whose key paid.
export const sourceText = (global: number, team: number) => `Tokens on global keys: ${count(global)} · on team keys: ${count(team)}`;

// TokensTotal is a total that can be reached with Tab: its name is the total
// and the 5 parts it adds up (a title shows them on hover).
export function TokensTotal({ tokens }: { tokens: UsageTokensView }) {
  const parts = partsText(tokens);
  return (
    <span role="group" tabIndex={0} title={parts} aria-label={`${count(total(tokens))} total: ${parts}`}>
      <b>{count(total(tokens))}</b>
    </span>
  );
}

// UsageCells is one usage as the cells under UsageHead. The total can be
// reached with Tab: its title and name list the 5 parts.
export function UsageCells({ tokens }: { tokens: UsageTokensView }) {
  return (
    <>
      <td className="num">{count(prompt(tokens))}</td>
      <td className="num">{count(output(tokens))}</td>
      <td className="num">
        <TokensTotal tokens={tokens} />
      </td>
      <td className="num meta">{cachedPercent(tokens)}</td>
    </>
  );
}

export function EmptyRow({ children }: { children: ReactNode }) {
  return <div className="row empty-row">{children}</div>;
}

// DailyTable is the days, each with its share of the busiest day as a
// progress bar (the CSP allows no inline style to draw one).
export function DailyTable({ daily }: { daily: UsageDayView[] }) {
  const peak = Math.max(1, ...daily.map((d) => total(d.tokens)));
  return (
    <section className="card scroll" aria-label="Daily">
      <h2>Daily</h2>
      {daily.length === 0 ? (
        <EmptyRow>No usage in this period.</EmptyRow>
      ) : (
        <table className="table">
          <thead>
            <UsageHead first="Day (UTC)">
              <th>Share of the busiest day</th>
            </UsageHead>
          </thead>
          <tbody>
            {daily.map((d) => (
              <tr key={d.day}>
                <td>{d.day}</td>
                <td>
                  <progress aria-label={`${d.day} share of the busiest day`} title={`Prompt ${count(prompt(d.tokens))} · Output ${count(output(d.tokens))}`} value={total(d.tokens)} max={peak} />
                </td>
                <UsageCells tokens={d.tokens} />
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </section>
  );
}
