import type { ReactNode } from "react";

import { paths, type UsageDayView, type UsageTokensView } from "../api";
import { count } from "../ui";

export const periods = [7, 30, 90];

// dash stands for a number that doesn't exist, such as the median of no reviews.
export const dash = "—";

// total is the tokens a total, a ranking and a median count: input plus output.
export const total = (t: UsageTokensView) => t.in + t.out;

// reviews says "1 review" or "3 reviews".
export const reviewsText = (n: number) => `${n} ${n === 1 ? "review" : "reviews"}`;

// apartText is what was reported apart from the totals. A pergent too old to
// report cache and reasoning leaves them unknown, which is never shown as
// zero: unknown counts the reviews it covers.
export function apartText(t: UsageTokensView): string {
  const parts: string[] = [];
  if (t.unknown === 0 || t.cacheRead + t.cacheWrite + t.reasoning > 0) {
    parts.push(`cache read ${count(t.cacheRead)}`, `cache write ${count(t.cacheWrite)}`, `reasoning ${count(t.reasoning)}`);
  }
  if (t.unknown > 0) parts.push(`unknown for ${reviewsText(t.unknown)}`);
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

// UsageNotes is the line both pages carry about how the numbers are made.
export function UsageNotes() {
  return (
    <p className="sub">
      Days are cut at midnight UTC. Totals add input and output tokens only; cache and reasoning counts are shown apart and are not in them.
    </p>
  );
}

// UsageHead is the column heads UsageCells fills; first is the head of the
// column before them.
export function UsageHead({ first, children }: { first: string; children?: ReactNode }) {
  return (
    <tr>
      <th>{first}</th>
      {children}
      <th className="num">Input</th>
      <th className="num">Output</th>
      <th className="num">Tokens</th>
      <th>Cache and reasoning</th>
    </tr>
  );
}

// UsageCells is one usage as the cells under UsageHead.
export function UsageCells({ tokens }: { tokens: UsageTokensView }) {
  return (
    <>
      <td className="num">{count(tokens.in)}</td>
      <td className="num">{count(tokens.out)}</td>
      <td className="num">
        <b>{count(total(tokens))}</b>
      </td>
      <td className="num meta">{apartText(tokens)}</td>
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
                  <progress aria-label={`${d.day} share of the busiest day`} value={total(d.tokens)} max={peak} />
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
