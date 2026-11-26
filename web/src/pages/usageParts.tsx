import type { ReactNode } from "react";

import { Button } from "@/components/ui/button";
import { Card, CardHeader, CardTitle } from "@/components/ui/card";
import { NativeSelect } from "@/components/ui/native-select";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { cn } from "@/lib/utils";

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

// unknownText says how many skill runs reported no cache or reasoning.
const unknownText = (n: number) => `unknown for ${skillRunsText(n)}`;

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
  if (t.unknown > 0) parts.push(unknownText(t.unknown));
  return parts.join(" · ");
}

// TokenTable is a run's tokens: the total, then the prompt and the output,
// each followed by the parts it adds up. The unknown runs end the prompt's
// parts, where the missing cache would have been.
export function TokenTable({ tokens }: { tokens: UsageTokensView }) {
  // A part is muted and set in under the sum it adds to.
  const row = (k: string, v: number, part = false) => (
    <tr className={part ? "text-muted-foreground" : undefined}>
      <th scope="row" className={cn("py-0.5 text-left", part ? "pl-3 font-normal" : "font-medium")}>
        {k}
      </th>
      <td className="py-0.5 text-right">{count(v)}</td>
    </tr>
  );
  return (
    <div className="flex flex-col gap-1">
      <div className="text-xl leading-tight font-semibold tabular-nums">{count(total(tokens))}</div>
      <table aria-label="Tokens" className="w-full max-w-72 text-sm tabular-nums">
        <tbody>
          {row("Prompt", prompt(tokens))}
          {row("cache read", tokens.cacheRead, true)}
          {row("cache write", tokens.cacheWrite, true)}
          {row("input", tokens.in, true)}
          {tokens.unknown > 0 && (
            <tr className="text-muted-foreground">
              <td colSpan={2} className="py-0.5 pl-3">
                {unknownText(tokens.unknown)}
              </td>
            </tr>
          )}
          {row("Output", output(tokens))}
          {row("output", tokens.out, true)}
          {row("reasoning", tokens.reasoning, true)}
        </tbody>
      </table>
    </div>
  );
}

// PeriodSelect is the period menu of the Usage pages.
export function PeriodSelect({ days, onChange }: { days: number; onChange: (d: number) => void }) {
  return (
    <NativeSelect aria-label="Period" className="w-40" value={days} onChange={(e) => onChange(Number(e.target.value))}>
      {periods.map((d) => (
        <option key={d} value={d}>
          Last {d} days
        </option>
      ))}
    </NativeSelect>
  );
}

// CsvLink is a plain download link: a GET on the same origin sends the
// session cookie, and it needs no CSRF token.
export function CsvLink({ days, instance, project }: { days: number; instance?: string; project?: string }) {
  return (
    <Button asChild variant="outline">
      <a href={paths.apiUsageCSV(days, instance, project)} download>
        Download CSV
      </a>
    </Button>
  );
}

// usageIntro says what a total counts; the Usage pages and the team tab share it.
export const usageIntro = "Totals count all tokens: input, cache read, cache write, output and reasoning.";

// UsageNotes is the line both pages carry about how the numbers are made.
export function UsageNotes() {
  return (
    <p className="text-muted-foreground mb-4 text-sm">
      Days are cut at midnight UTC. {usageIntro}
    </p>
  );
}

// UsageStats is the 4 cards on top of a Usage page, and the lines that say
// what Prompt and Output hold.
export function UsageStats({ tokens, reviews }: { tokens: UsageTokensView; reviews: number }) {
  const cached = cachedPercent(tokens);
  return (
    <div className="flex flex-col gap-2">
      <section className="grid grid-cols-1 gap-4 sm:grid-cols-2 xl:grid-cols-4" aria-label="Totals">
        <Stat k="Tokens" v={count(total(tokens))} d="all token kinds added" />
        <Stat k="Prompt" v={count(prompt(tokens))} d={cached === dash ? "cache share unknown" : `${cached} from cache`} />
        <Stat k="Output" v={count(output(tokens))} d="incl. reasoning" />
        <Stat k="Reviews" v={String(reviews)} d="with usage recorded" />
      </section>
      <div className="text-muted-foreground mt-1 text-sm">
        <p>Prompt: input · cache read · cache write</p>
        <p>Output: output · reasoning</p>
      </div>
    </div>
  );
}

function Stat({ k, v, d }: { k: string; v: string; d: string }) {
  return (
    // "stat" stays as a class name: the tests read the cards by it.
    <Card className="stat gap-1 px-4 py-4">
      <div className="text-muted-foreground text-sm">{k}</div>
      <div className="text-3xl font-semibold tracking-tight tabular-nums">{v}</div>
      <div className="text-muted-foreground text-xs">{d}</div>
    </Card>
  );
}

// num is a column of numbers: right-aligned, every digit the same width.
export const num = "text-right tabular-nums";

// usageTable is a usage card's table: the first and last columns line up
// with the card's title, and the first column's names stay on one line (the
// table scrolls sideways on a phone instead).
export const usageTable = "[&_tr>*:first-child]:pl-4 [&_tr>*:last-child]:pr-4 [&_td:first-child]:whitespace-nowrap";

// UsageCard is a card of the Usage pages: its title, then its table or its
// empty line under a line of its own.
export function UsageCard({ label, title, children }: { label: string; title?: string; children: ReactNode }) {
  return (
    <Card role="region" aria-label={label} className="gap-0 overflow-hidden py-0">
      <CardHeader className="py-3">
        <CardTitle>{title ?? label}</CardTitle>
      </CardHeader>
      <div className="border-t">{children}</div>
    </Card>
  );
}

// UsageHead is the column heads UsageCells fills; first is the head of the
// column before them, children the heads between, after those after them.
export function UsageHead({ first, children, after }: { first: string; children?: ReactNode; after?: ReactNode }) {
  return (
    <TableRow className="hover:bg-transparent">
      <TableHead>{first}</TableHead>
      {children}
      <TableHead className={num}>Prompt</TableHead>
      <TableHead className={num}>Output</TableHead>
      <TableHead className={num}>Tokens</TableHead>
      <TableHead className={num}>Cached</TableHead>
      {after}
    </TableRow>
  );
}

// sourceText is a usage's tokens split by whose key paid.
export const sourceText = (global: number, team: number) => `Tokens on global keys: ${count(global)} · on team keys: ${count(team)}`;

// TokensTotal is a total that can be reached with Tab: its name is the total
// and the 5 parts it adds up (a title shows them on hover).
export function TokensTotal({ tokens }: { tokens: UsageTokensView }) {
  const parts = partsText(tokens);
  return (
    <span
      role="group"
      tabIndex={0}
      title={parts}
      aria-label={`${count(total(tokens))} total: ${parts}`}
      className="focus-visible:ring-ring/50 rounded-sm font-semibold outline-none focus-visible:ring-[3px]"
    >
      {count(total(tokens))}
    </span>
  );
}

// UsageCells is one usage as the cells under UsageHead. The total can be
// reached with Tab: its title and name list the 5 parts.
export function UsageCells({ tokens }: { tokens: UsageTokensView }) {
  return (
    <>
      <TableCell className={num}>{count(prompt(tokens))}</TableCell>
      <TableCell className={num}>{count(output(tokens))}</TableCell>
      <TableCell className={num}>
        <TokensTotal tokens={tokens} />
      </TableCell>
      <TableCell className={cn(num, "text-muted-foreground")}>{cachedPercent(tokens)}</TableCell>
    </>
  );
}

export function EmptyRow({ children }: { children: ReactNode }) {
  return <div className="text-muted-foreground px-4 py-6 text-sm">{children}</div>;
}

// DailyTable is the days, each with its share of the busiest day as a
// progress bar (the CSP allows no inline style to draw one).
export function DailyTable({ daily }: { daily: UsageDayView[] }) {
  const peak = Math.max(1, ...daily.map((d) => total(d.tokens)));
  return (
    <UsageCard label="Daily">
      {daily.length === 0 ? (
        <EmptyRow>No usage in this period.</EmptyRow>
      ) : (
        <Table className={usageTable}>
          <TableHeader className="bg-muted/40">
            <UsageHead first="Day (UTC)">
              <TableHead>Share of the busiest day</TableHead>
            </UsageHead>
          </TableHeader>
          <TableBody>
            {daily.map((d) => (
              <TableRow key={d.day}>
                <TableCell>{d.day}</TableCell>
                <TableCell>
                  <progress
                    className="accent-primary w-full min-w-24"
                    aria-label={`${d.day} share of the busiest day`}
                    title={`Prompt ${count(prompt(d.tokens))} · Output ${count(output(d.tokens))}`}
                    value={total(d.tokens)}
                    max={peak}
                  />
                </TableCell>
                <UsageCells tokens={d.tokens} />
              </TableRow>
            ))}
          </TableBody>
        </Table>
      )}
    </UsageCard>
  );
}
