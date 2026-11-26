import { useInfiniteQuery } from "@tanstack/react-query";
import { Fragment, type ReactNode } from "react";
import { Link } from "react-router";

import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";

import { api, paths, query, type AuditPageView, type AuditView } from "../api";
import { ErrorBox, Loading, PageHeader, Pill, SearchField, type Tone } from "../ui";
import { useSearchText } from "../useSearchText";

// day is an RFC 3339 time's UTC day, as "Wed 25 Nov 2026" (the API sends
// UTC), spelled out here so every browser shows the same.
const weekdays = ["Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"];
const months = ["Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"];
const day = (at: string) => {
  const d = new Date(at);
  if (Number.isNaN(d.getTime())) return at.slice(0, 10);
  return `${weekdays[d.getUTCDay()]} ${d.getUTCDate()} ${months[d.getUTCMonth()]} ${d.getUTCFullYear()}`;
};

// clock is its UTC time of day, as "14:02".
const clock = (at: string) => at.slice(11, 16);

// what is an action like "team_created" as "Team created".
const what = (action: string) => {
  const text = action.replaceAll("_", " ");
  return text.charAt(0).toUpperCase() + text.slice(1);
};

// toneOf colours an action by what it did: red for what took something
// away, amber for what was refused or failed, grey for the rest.
const toneOf = (action: string): Tone => {
  if (/(^|_)(removed|deleted|revoked|stopped|off)$/.test(action)) return "bad";
  if (/(^|_)(refused|failed|locked)$/.test(action)) return "warn";
  return "grey";
};

// Who is an actor label: the name, and on a second line where it is from,
// such as "gitlab main" (a command's author) or "password" (a local admin).
function Who({ actor }: { actor: string }) {
  const m = /^(.*) \(([^()]+)\)$/.exec(actor);
  if (!m) return <>{actor}</>;
  return (
    <>
      {m[1]}
      <div className="meta text-muted-foreground text-xs">{m[2]}</div>
    </>
  );
}

// Where is the repository an entry is about, as a link to its page, or the
// instance alone.
function Where({ e }: { e: AuditView }) {
  if (e.project === "") return <>{e.instance}</>;
  return (
    <Link className="text-foreground no-underline hover:underline" to={paths.repo(e.instance, e.project)} title={`${e.instance}/${e.project}`}>
      {e.project}
      <div className="meta text-muted-foreground text-xs">{e.instance}</div>
    </Link>
  );
}

// A detail of "key=value" pairs (a command's) is shown pair by pair: a long
// commit SHA shortened to 8 characters (the whole one on hover), and mr=12
// linked to the review page. Any other detail is text, wrapped.
const pairs = /^\S+=\S*( \S+=\S*)*$/;
function Detail({ e }: { e: AuditView }) {
  if (!pairs.test(e.detail)) return <span>{e.detail}</span>;
  return (
    <span className="flex flex-wrap gap-x-3 gap-y-1">
      {e.detail.split(" ").map((pair, i) => {
        const at = pair.indexOf("=");
        const key = pair.slice(0, at);
        const value = pair.slice(at + 1);
        let shown: ReactNode = value;
        if (/^[0-9a-f]{12,}$/i.test(value)) {
          shown = (
            <abbr className="font-mono text-xs no-underline" title={value}>
              {value.slice(0, 8)}
            </abbr>
          );
        } else if ((key === "mr" || key === "pr") && /^\d+$/.test(value) && e.project !== "") {
          shown = <Link to={paths.review(e.instance, e.project, Number(value))}>{value}</Link>;
        }
        return (
          <span key={i}>
            <span className="text-muted-foreground">{key}</span> {shown}
          </span>
        );
      })}
    </span>
  );
}

// AuditLog is the record of who changed what, newest first, a day at a
// time; the search looks through the whole log on the server, and Load more
// reads the page before the last one shown.
export default function AuditLog() {
  const [typed, setTyped, q] = useSearchText();
  const log = useInfiniteQuery({
    queryKey: ["audit", q],
    queryFn: ({ pageParam }) =>
      api.get<AuditPageView>(`/api/audit${query([["q", q], ["before", pageParam || undefined]])}`),
    initialPageParam: 0,
    // A next of 0 is the end of the log.
    getNextPageParam: (last) => last.next || undefined,
  });
  // A failed Load more leaves the query in error with the pages already read:
  // they stay, and so does the button to try again.
  const entries = log.data?.pages.flatMap((p) => p.entries) ?? [];
  return (
    <div className="max-w-[1100px]">
      <PageHeader title="Audit log" description="Every change made in the dashboard, kept for 90 days. Never a secret. Times are UTC." />
      <Card className="mb-4 gap-0 overflow-hidden py-0">
        <div className="flex flex-wrap items-center gap-2 border-b px-4 py-3">
          <SearchField
            label="Search the audit log"
            placeholder="Search by who, what, repository or detail"
            value={typed}
            onChange={setTyped}
          />
        </div>
        {log.isPending && <Loading />}
        {log.isError && !log.isFetchNextPageError && <ErrorBox error={log.error} onRetry={() => log.refetch()} />}
        {log.data &&
          (entries.length === 0 && !log.hasNextPage ? (
            <div className="text-muted-foreground px-4 py-8 text-center text-sm">
              {q === "" ? "Nothing is recorded yet." : `No entry holds "${q}". Try a shorter search, or clear it.`}
            </div>
          ) : (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Time</TableHead>
                  <TableHead>Who</TableHead>
                  <TableHead>What</TableHead>
                  <TableHead>Where</TableHead>
                  <TableHead>Detail</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {entries.map((e, i) => {
                  const d = day(e.at);
                  const first = i === 0 || day(entries[i - 1].at) !== d;
                  return (
                    <Fragment key={e.id}>
                      {first && (
                        <TableRow className="day-row bg-muted/50 hover:bg-muted/50">
                          <TableHead colSpan={5} scope="colgroup" className="text-foreground h-8 px-4 text-xs font-semibold">
                            {d}
                          </TableHead>
                        </TableRow>
                      )}
                      <TableRow className="[&>td]:align-top">
                        <TableCell className="whitespace-nowrap">
                          <time dateTime={e.at} title={`${e.at.slice(0, 10)} ${clock(e.at)} UTC`}>
                            {clock(e.at)}
                          </time>
                        </TableCell>
                        <TableCell>
                          <Who actor={e.actor} />
                        </TableCell>
                        <TableCell className="whitespace-nowrap">
                          <Pill tone={toneOf(e.action)}>{what(e.action)}</Pill>
                        </TableCell>
                        <TableCell>
                          <Where e={e} />
                        </TableCell>
                        <TableCell className="max-w-[420px] min-w-[220px] [overflow-wrap:anywhere]">
                          <Detail e={e} />
                        </TableCell>
                      </TableRow>
                    </Fragment>
                  );
                })}
              </TableBody>
            </Table>
          ))}
        {log.hasNextPage && (
          <div className="flex justify-end border-t px-4 py-3">
            <Button type="button" variant="outline" size="sm" disabled={log.isFetchingNextPage} onClick={() => log.fetchNextPage()}>
              Load more
            </Button>
          </div>
        )}
      </Card>
      {log.isFetchNextPageError && <ErrorBox error={log.error} />}
    </div>
  );
}
