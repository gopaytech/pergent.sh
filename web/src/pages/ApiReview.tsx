import { useQuery } from "@tanstack/react-query";
import { useState, type ReactNode } from "react";
import { Link, useParams } from "react-router";

import {
  api,
  isStatus,
  paths,
  type APICallback,
  type APIFinding,
  type APIReviewDetail,
  type APISkill,
  type APIStatus,
  type APIVerdict,
} from "../api";
import RunLog from "../RunLog";
import { ErrorBox, Loading, NotFound, Pill, Tabs, count, utcText, type Tone } from "../ui";

const statuses: Record<APIStatus, [Tone, string]> = {
  queued: ["grey", "Queued"],
  running: ["run", "Running"],
  completed: ["ok", "Completed"],
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
  const [tab, setTab] = useState<Tab>("review");
  const review = useQuery({
    queryKey: ["api-review", id],
    queryFn: () => api.get<APIReviewDetail>(paths.apiAdminReview(id)),
  });
  if (review.isPending) return <Loading />;
  if (isStatus(review.error, 404)) return <NotFound />;
  if (review.isError) return <ErrorBox error={review.error} />;
  const view = review.data;
  return (
    <div className="narrow">
      <div className="top">
        <div>
          <h1>{view.title || view.subject}</h1>
          <div className="sub">
            <Link to="/api-clients">API clients</Link> / <Link to={paths.clientPage(view.client)}>{view.client}</Link>
          </div>
        </div>
      </div>
      <Facts view={view} />
      <Tabs
        tabs={[
          ["review", "Review"],
          ["logs", "Logs"],
        ]}
        value={tab}
        onChange={setTab}
      />
      <div role="tabpanel">
        {tab === "review" ? (
          <Result view={view} />
        ) : (
          <section className="card" aria-label="Log">
            <RunLog logPath={paths.apiAdminReviewLog(view.id)} diagnosticsPath={paths.apiAdminReviewDiagnostics(view.id)} />
          </section>
        )}
      </div>
    </div>
  );
}

function Facts({ view }: { view: APIReviewDetail }) {
  const { review } = view;
  const usage = review.usage;
  return (
    <dl className="card kvs" role="group" aria-label="Review">
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
          {count(usage.tokens_in)} in, {count(usage.tokens_out)} out
        </Fact>
      )}
      {review.test && <Fact k="Test review">no change was reviewed</Fact>}
    </dl>
  );
}

function Fact({ k, children }: { k: string; children: ReactNode }) {
  return (
    <div className="kv">
      <dt>{k}</dt>
      <dd>{children}</dd>
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
        <div className="error" role="alert">
          Failed at {failure.step}: {failure.reason}
          {failure.detail && <pre className="log">{failure.detail}</pre>}
        </div>
      )}
      {skills.length === 0 && !failure && <div className="card empty">{noResult(view.status)}</div>}
      {skills.map((s) => (
        <Skill key={s.name} skill={s} />
      ))}
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
    <section className="card pad" aria-label="Callback">
      <h3 className="group">Callback</h3>
      <div className="meta">{callbackStates[callback.state] ?? callback.state}</div>
      <ul className="files">
        {callback.attempts.map((a, i) => (
          <li key={`${a.at}/${i}`}>
            <span className="meta">{utcText(a.at)}</span>{" "}
            {a.statusCode === 0 ? `No answer: ${a.error}` : `Answered ${a.statusCode}${a.error ? `. ${a.error}` : ""}`}{" "}
            <span className="meta">{a.durationMs} ms</span>
          </li>
        ))}
      </ul>
    </section>
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

function Skill({ skill }: { skill: APISkill }) {
  const findings = [...skill.findings].sort((a, b) => rank(a) - rank(b));
  const { reviewed, skipped, not_reviewed: notReviewed } = skill.files;
  return (
    <section className="card" aria-label={`Skill ${skill.name}`}>
      <div className="verdict">
        <VerdictPill verdict={skill.verdict} />
        <div className="grow">
          <b>{skill.name}</b>
          <div className="meta">{findings.length === 1 ? "1 finding" : `${findings.length} findings`}</div>
        </div>
      </div>
      {findings.map((f, i) => (
        <Finding key={`${f.id}/${i}`} f={f} />
      ))}
      <div className="pad">
        <h3 className="group">Reviewed ({reviewed.length})</h3>
        {reviewed.length > 0 && (
          <div role="group" aria-label="Reviewed files">
            <ul className="files">
              {reviewed.map((p) => (
                <li key={p} className="mono">
                  {p}
                </li>
              ))}
            </ul>
          </div>
        )}
        <h3 className="group">Skipped ({skipped.length})</h3>
        {skipped.length > 0 && (
          <div role="group" aria-label="Skipped files">
            <ul className="files">
              {skipped.map((s) => (
                <li key={s.path}>
                  <span className="mono">{s.path}</span> <span className="meta">{s.rule}</span>
                </li>
              ))}
            </ul>
          </div>
        )}
        {notReviewed.length > 0 && (
          <>
            <h3 className="group">Not reviewed ({notReviewed.length})</h3>
            <div role="group" aria-label="Files not reviewed">
              <ul className="files">
                {notReviewed.map((s) => (
                  <li key={s.path}>
                    <span className="mono">{s.path}</span> <span className="meta">{s.reason}</span>
                  </li>
                ))}
              </ul>
            </div>
          </>
        )}
      </div>
    </section>
  );
}

function Finding({ f }: { f: APIFinding }) {
  const where = f.line ? `${f.file}:${f.line}${f.end_line && f.end_line > f.line ? `-${f.end_line}` : ""}` : f.file;
  return (
    <div className="finding">
      <Pill tone={f.severity === "blocking" ? "bad" : "grey"}>{f.severity}</Pill> <span className="t">{f.title}</span>{" "}
      <span className="mono meta">{f.id}</span>
      <div className="loc">{where}</div>
      {f.explanation && <p className="prose">{f.explanation}</p>}
      {f.fix && (
        <>
          <div className="fix-label">Suggested fix</div>
          <p className="prose">{f.fix}</p>
        </>
      )}
      {f.suggestion && (
        <>
          <div className="fix-label">Replacement</div>
          <pre className="fix">{f.suggestion}</pre>
        </>
      )}
      {f.dismissed && (
        <div className="meta">
          Dismissed: {f.dismissed.reason} · {utcText(f.dismissed.at)}
        </div>
      )}
    </div>
  );
}
