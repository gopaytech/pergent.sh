import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent, type ReactNode } from "react";
import { Link, useParams } from "react-router";

import {
  api,
  isStatus,
  paths,
  type APICallback,
  type APIDismissStatus,
  type APIFinding,
  type APIReviewDetail,
  type APISkill,
  type APIStatus,
  type APIReviewBody,
  type APIVerdict,
  type UsageTokensView,
} from "../api";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Card, CardHeader, CardTitle } from "@/components/ui/card";
import { Textarea } from "@/components/ui/textarea";
import { cn } from "@/lib/utils";

import { useWrite } from "../layout/write";
import RunLog from "../RunLog";
import { ErrorBox, Loading, NotFound, PageHeader, Pill, Tabs, utcText, type Tone } from "../ui";
import { useUrlParam } from "../urlState";
import { TokenTable } from "./usageParts";

// The class names the page's cards share.
// listCard is a card of rows under a title; each row draws its own top line.
const listCard = "gap-0 overflow-hidden py-0";
// line is one row or block of a card, under a line.
const line = "border-t px-4 py-3 text-sm";
// muted is a note in the card's smaller, lighter text.
const muted = "text-muted-foreground text-xs";
// groupHead is the small heading over a group of rows in a card.
const groupHead = "text-muted-foreground text-[13px] font-semibold";
// codeBox is model or Job text shown as written, in a box of its own.
const codeBox = "bg-muted/50 rounded-md border px-3 py-2.5 font-mono text-xs break-words whitespace-pre-wrap";
// crumb is a link of the path under the title.
const crumb = "text-muted-foreground hover:text-foreground hover:underline";

const statuses: Record<APIStatus, [Tone, string]> = {
  queued: ["grey", "Queued"],
  running: ["run", "Running"],
  completed: ["grey", "Completed"],
  failed: ["warn", "Failed"],
  superseded: ["grey", "Superseded"],
};

// The tones are those of OutcomePill: no blocking is ok, blocking is bad, a
// failure is warn. A review is never "approved": the API only reports.
const verdicts: Record<APIVerdict, [Tone, string]> = {
  no_blocking_issues: ["ok", "No blocking issues"],
  has_blocking_issues: ["bad", "Blocking issues"],
  nothing_reviewed: ["grey", "Nothing reviewed"],
  failed: ["warn", "Failed"],
};

export function StatusPill({ status }: { status: string }) {
  const [tone, label] = statuses[status as APIStatus] ?? ["grey", status];
  return <Pill tone={tone}>{label}</Pill>;
}

export function VerdictPill({ verdict }: { verdict: string }) {
  const [tone, label] = verdicts[verdict as APIVerdict] ?? ["grey", verdict];
  return <Pill tone={tone}>{label}</Pill>;
}

type Tab = "review" | "logs";

// ApiReview is one review of the review API, for an admin: who asked, the
// status and verdict, each skill's findings and files, and the log. Nothing
// here runs a review again: the API's caller sends a change to review again.
export default function ApiReview() {
  const { id = "" } = useParams();
  const [tab, setTab] = useUrlParam<Tab>("tab", "review", ["review", "logs"]);
  const review = useQuery({
    queryKey: ["api-review", id],
    queryFn: () => api.get<APIReviewDetail>(paths.apiAdminReview(id)),
  });
  if (review.isPending) return <Loading />;
  if (isStatus(review.error, 404)) return <NotFound />;
  if (review.isError) return <ErrorBox error={review.error} />;
  const view = review.data;
  return (
    <div className="max-w-[1100px]">
      <PageHeader
        title={view.title || view.subject}
        description={
          <>
            <Link className={crumb} to="/api-clients">
              API clients
            </Link>{" "}
            /{" "}
            <Link className={crumb} to={paths.clientPage(view.client)}>
              {view.client}
            </Link>
          </>
        }
      />
      <Facts view={view} />
      <Tabs
        tabs={[
          ["review", "Review"],
          ["logs", "Logs"],
        ]}
        value={tab}
        onChange={setTab}
      />
      <div role="tabpanel" className="flex flex-col gap-4">
        {tab === "review" ? (
          <Result view={view} />
        ) : (
          <Card role="region" aria-label="Log" className="gap-0 py-1">
            <RunLog logPath={paths.apiAdminReviewLog(view.id)} diagnosticsPath={paths.apiAdminReviewDiagnostics(view.id)} />
          </Card>
        )}
      </div>
    </div>
  );
}

function Facts({ view }: { view: APIReviewDetail }) {
  const { review } = view;
  const usage = review.usage;
  return (
    <Card role="group" aria-label="Review" className={cn(listCard, "mb-4")}>
      <dl>
        <Fact k="Subject">{view.subject}</Fact>
        <Fact k="Tenant">{view.tenant || "none"}</Fact>
        <Fact k="Key">{view.keyName}</Fact>
        <Fact k="Model">{view.model ?? "not chosen"}</Fact>
        <Fact k="Status">
          <StatusPill status={view.status} />
        </Fact>
        {review.verdict && (
          <Fact k="Verdict">
            <VerdictPill verdict={review.verdict} />
          </Fact>
        )}
        <Fact k="Created">{utcText(view.createdAt)}</Fact>
        {view.finishedAt && <Fact k="Finished">{utcText(view.finishedAt)}</Fact>}
        {usage && (
          <Fact k="Tokens">
            <TokenTable tokens={apiTokens(usage)} />
          </Fact>
        )}
        {review.test && <Fact k="Test review">no change was reviewed</Fact>}
      </dl>
    </Card>
  );
}

// apiTokens reads the Review API's usage as the tokens of a run. Its parts
// are already counted as 0 when pergent did not report them, so none is unknown.
// A review that finished before all tokens were counted has only tokens_in and
// tokens_out: the missing parts are 0 and the total is the sum of the parts.
const apiTokens = (u: NonNullable<APIReviewBody["usage"]>): UsageTokensView => {
  const cacheRead = u.cache_read ?? 0;
  const cacheWrite = u.cache_write ?? 0;
  const reasoning = u.reasoning ?? 0;
  const prompt = u.tokens_in + cacheRead + cacheWrite;
  const output = u.tokens_out + reasoning;
  return {
    in: u.tokens_in,
    out: u.tokens_out,
    cacheRead,
    cacheWrite,
    reasoning,
    unknown: 0,
    prompt,
    output,
    total: u.tokens ?? prompt + output,
  };
};

function Fact({ k, children }: { k: string; children: ReactNode }) {
  return (
    // A name on the left and its value on the right; one under the other on
    // a phone.
    <div className="grid gap-x-4 gap-y-1 border-t px-4 py-2.5 text-sm first:border-t-0 sm:grid-cols-[180px_minmax(0,1fr)]">
      <dt className="text-muted-foreground">{k}</dt>
      <dd className="min-w-0 font-medium">{children}</dd>
    </div>
  );
}

// Result is the failure, if any, and the skills. The failure's detail is
// for admins; the reason is what the caller is told.
function Result({ view }: { view: APIReviewDetail }) {
  const failure = view.failure ?? (view.review.failure ? { ...view.review.failure, detail: "" } : null);
  const skills = view.review.skills ?? [];
  return (
    <>
      {failure && (
        <Alert variant="destructive">
          <AlertTitle className="line-clamp-none">
            Failed at {failure.step}: {failure.reason}
          </AlertTitle>
          {failure.detail && (
            <AlertDescription>
              <pre className={cn(codeBox, "bg-card text-foreground max-h-80 w-full overflow-auto")}>{failure.detail}</pre>
            </AlertDescription>
          )}
        </Alert>
      )}
      {skills.length === 0 && !failure && <Card className="text-muted-foreground px-4 py-6 text-sm">{noResult(view.status)}</Card>}
      {skills.map((s) => (
        <Skill key={s.name} skill={s} view={view} />
      ))}
      {view.history.length > 0 && <History view={view} />}
      {view.callback.state !== "none" && <Callback callback={view.callback} />}
    </>
  );
}

const callbackStates: Record<string, string> = {
  pending: "Will be tried again.",
  delivered: "Delivered.",
  gave_up: "Given up.",
};

// Callback is the review's callback to its client: where it stands and every
// try, oldest first. The error is the redacted text pergent.sh stored.
function Callback({ callback }: { callback: APICallback }) {
  return (
    <Card role="region" aria-label="Callback" className={listCard}>
      <CardHeader className="py-3">
        <CardTitle>Callback</CardTitle>
      </CardHeader>
      <div className={line}>
        <div className="text-muted-foreground">{callbackStates[callback.state] ?? callback.state}</div>
        <ul className="mt-1.5 flex flex-col gap-1">
          {callback.attempts.map((a, i) => (
            <li key={`${a.at}/${i}`} className="wrap-anywhere">
              <span className={muted}>{utcText(a.at)}</span>{" "}
              {a.statusCode === 0 ? `No answer: ${a.error}` : `Answered ${a.statusCode}${a.error ? `. ${a.error}` : ""}`}{" "}
              <span className={muted}>{a.durationMs} ms</span>
            </li>
          ))}
        </ul>
      </div>
    </Card>
  );
}

function noResult(status: string): string {
  switch (status) {
    case "queued":
    case "running":
      return "No result yet.";
    case "superseded":
      return "This review was superseded, so it has no result.";
  }
  return "The review has no skill results.";
}

// rank puts the findings that matter first: blocking, then suggestions,
// then the dismissed ones.
const rank = (f: APIFinding) => (f.dismissed ? 2 : f.severity === "blocking" ? 0 : 1);

function Skill({ skill, view }: { skill: APISkill; view: APIReviewDetail }) {
  const findings = [...skill.findings].sort((a, b) => rank(a) - rank(b));
  const { reviewed, skipped, not_reviewed: notReviewed } = skill.files;
  return (
    <Card role="region" aria-label={`Skill ${skill.name}`} className={listCard}>
      <div className="flex items-center gap-2.5 px-4 py-3">
        <VerdictPill verdict={skill.verdict} />
        <div className="min-w-0 grow text-sm">
          <b className="font-semibold">{skill.name}</b>
          <div className={muted}>{findings.length === 1 ? "1 finding" : `${findings.length} findings`}</div>
        </div>
      </div>
      {findings.map((f, i) => (
        <Finding key={`${f.id}/${i}`} f={f} view={view} />
      ))}
      <div className={line}>
        <h3 className={cn(groupHead, "mb-1.5")}>Reviewed ({reviewed.length})</h3>
        {reviewed.length > 0 && (
          <div role="group" aria-label="Reviewed files">
            <ul className={fileList}>
              {reviewed.map((p) => (
                <li key={p} className={cn(fileItem, "font-mono text-xs")}>
                  {p}
                </li>
              ))}
            </ul>
          </div>
        )}
        <h3 className={cn(groupHead, "mt-3 mb-1.5")}>Skipped ({skipped.length})</h3>
        {skipped.length > 0 && (
          <div role="group" aria-label="Skipped files">
            <ul className={fileList}>
              {skipped.map((s) => (
                <li key={s.path} className={fileItem}>
                  <span className="font-mono text-xs">{s.path}</span> <span className={muted}>{s.rule}</span>
                </li>
              ))}
            </ul>
          </div>
        )}
        {notReviewed.length > 0 && (
          <>
            <h3 className={cn(groupHead, "mt-3 mb-1.5")}>Not reviewed ({notReviewed.length})</h3>
            <div role="group" aria-label="Files not reviewed">
              <ul className={fileList}>
                {notReviewed.map((s) => (
                  <li key={s.path} className={fileItem}>
                    <span className="font-mono text-xs">{s.path}</span> <span className={muted}>{s.reason}</span>
                  </li>
                ))}
              </ul>
            </div>
          </>
        )}
      </div>
    </Card>
  );
}

// fileList is a list of file paths, in two columns on a wide screen; a long
// path wraps in its column, never cut.
const fileList = "mt-1 mb-1 text-sm sm:columns-2";
const fileItem = "break-inside-avoid py-0.5 wrap-anywhere";

// Finding is a finding of the review. A completed review's finding can be
// dismissed for the subject, and a finding dismissed for it can be stopped.
// What is dismissed now comes from the page's dismissals, not from the stored
// body, which a dismissal leaves as it was.
function Finding({ f, view }: { f: APIFinding; view: APIReviewDetail }) {
  const where = f.line ? `${f.file}:${f.line}${f.end_line && f.end_line > f.line ? `-${f.end_line}` : ""}` : f.file;
  const now = view.dismissals.find((d) => d.fingerprint === f.fingerprint);
  const may = view.status === "completed";
  return (
    // "finding" stays as a name the tests find a finding by.
    <div className={cn("finding", line)}>
      <Pill tone={f.severity === "blocking" ? "bad" : "grey"}>{f.severity}</Pill> <span className="font-semibold">{f.title}</span>{" "}
      <span className="text-muted-foreground font-mono text-xs">{f.id}</span>
      <div className="text-muted-foreground mt-0.5 font-mono text-xs wrap-anywhere">{where}</div>
      {f.explanation && <p className="prose mt-1.5">{f.explanation}</p>}
      {f.fix && (
        <>
          <div className={cn(muted, "mt-2")}>Suggested fix</div>
          <p className="prose">{f.fix}</p>
        </>
      )}
      {f.suggestion && (
        <>
          <div className={cn(muted, "mt-2 mb-1")}>Replacement</div>
          <pre className={codeBox}>{f.suggestion}</pre>
        </>
      )}
      {now ? (
        <div className="mt-1.5 border-l-[3px] pl-2.5">
          <div className={muted}>
            Dismissed by {now.actor} · {utcText(now.createdAt)}
          </div>
          <div className="prose">Reason: {now.reason}</div>
        </div>
      ) : (
        f.dismissed && (
          <div className={cn(muted, "mt-1.5")}>
            Dismissed: {f.dismissed.reason} · {utcText(f.dismissed.at)}
          </div>
        )
      )}
      {may && f.id !== "" && (now ? <StopControl view={view} f={f} /> : <DismissControl view={view} f={f} />)}
    </div>
  );
}

// useDismissal is the write that dismisses, or with method DELETE stops
// dismissing, a finding for the review's subject, and reads the review again.
function useDismissal(view: APIReviewDetail, f: APIFinding, method: "POST" | "DELETE", done: () => void) {
  const cache = useQueryClient();
  const write = useWrite();
  return useMutation({
    mutationFn: (reason: string) =>
      write<APIDismissStatus>(method, paths.apiAdminReviewDismiss(view.id, f.id), method === "POST" ? { reason } : undefined),
    onSuccess: async () => {
      done();
      await cache.invalidateQueries({ queryKey: ["api-review", view.id] });
    },
  });
}

// DismissControl is the Dismiss button and the form it opens. A reason is
// required (the server refuses one over 500 characters).
function DismissControl({ view, f }: { view: APIReviewDetail; f: APIFinding }) {
  const [open, setOpen] = useState(false);
  const [reason, setReason] = useState("");
  const [problem, setProblem] = useState("");
  const dismiss = useDismissal(view, f, "POST", () => {
    setOpen(false);
    setReason("");
  });
  const submit = (e: FormEvent) => {
    e.preventDefault();
    const why = reason.trim();
    if (why === "") {
      setProblem("Dismissing a finding needs a reason.");
      return;
    }
    setProblem("");
    dismiss.mutate(why);
  };
  if (!open) {
    return (
      <div className="mt-2">
        <Button type="button" variant="outline" size="sm" onClick={() => setOpen(true)}>
          Dismiss
        </Button>
      </div>
    );
  }
  return (
    <form className="mt-2 flex flex-col gap-2" onSubmit={submit}>
      <Textarea
        aria-label="Reason"
        placeholder="Why is this not a problem? (required)"
        rows={2}
        maxLength={500}
        value={reason}
        disabled={dismiss.isPending}
        onChange={(e) => setReason(e.target.value)}
      />
      <div className="flex flex-wrap items-center gap-2">
        <Button type="submit" size="sm" disabled={dismiss.isPending}>
          Dismiss finding
        </Button>
        <Button type="button" variant="outline" size="sm" disabled={dismiss.isPending} onClick={() => setOpen(false)}>
          Cancel
        </Button>
        {problem && <span className="text-bad text-xs">{problem}</span>}
      </div>
      {dismiss.isError && <ErrorBox error={dismiss.error} />}
    </form>
  );
}

// StopControl takes the dismissal back, so later reviews of the subject
// report the finding again.
function StopControl({ view, f }: { view: APIReviewDetail; f: APIFinding }) {
  const stop = useDismissal(view, f, "DELETE", () => {});
  return (
    <div className="mt-2 flex flex-col items-start gap-2">
      <Button type="button" variant="outline" size="sm" disabled={stop.isPending} onClick={() => stop.mutate("")}>
        Restore
      </Button>
      {stop.isError && <ErrorBox error={stop.error} />}
    </div>
  );
}

// History is every dismissal and restore made on this review, oldest first,
// with who did it. actor, title and reason are text people or the model wrote.
function History({ view }: { view: APIReviewDetail }) {
  return (
    <Card role="region" aria-label="History" className={listCard}>
      <CardHeader className="py-3">
        <CardTitle>History</CardTitle>
      </CardHeader>
      <ul className={cn(line, "flex flex-col gap-1.5")}>
        {view.history.map((e, i) => (
          <li key={`${e.at}/${i}`} className="wrap-anywhere">
            <span className={muted}>{utcText(e.at)}</span> {e.kind === "dismissed" ? "Dismissed" : "Stopped dismissing"}{" "}
            <b className="font-semibold">{e.title}</b> <span className={muted}>by {e.actor}</span>
            {e.reason && <div className="prose">Reason: {e.reason}</div>}
          </li>
        ))}
      </ul>
    </Card>
  );
}
