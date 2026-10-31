import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";
import { Link, useParams, useSearchParams } from "react-router";

import {
  api,
  isStatus,
  paths,
  type Can,
  type CheckView,
  type FindingView,
  type LogView,
  type RerunStatus,
  type ReviewDetailView,
  type ReviewHeader,
  type RunDetailView,
  type RunSummary,
  type SkillView,
} from "../api";
import { useWrite } from "../layout/write";
import {
  ErrorBox,
  ExternalLink,
  Loading,
  NotFound,
  OutcomePill,
  Pill,
  Tabs,
  ago,
  changeRef,
  count,
  duration,
  platformName,
  type Tone,
} from "../ui";

const triggers: Record<string, string> = {
  first: "first run",
  push: "push",
  retarget: "new target branch",
  rerun: "re-run",
};

// splitChange reads "{project}/{number}" from the route's rest.
function splitChange(rest: string): { project: string; number: number } | null {
  const i = rest.lastIndexOf("/");
  const n = rest.slice(i + 1);
  if (i <= 0 || !/^[1-9]\d{0,14}$/.test(n)) return null;
  return { project: rest.slice(0, i), number: Number(n) };
}

// ReviewDetail is one merge request or pull request: its header, every run,
// and the selected run's skills, findings, checks, files and log.
export default function ReviewDetail() {
  const params = useParams();
  const instance = params.instance ?? "";
  const change = splitChange(params["*"] ?? "");
  const review = useQuery({
    queryKey: ["review", instance, change?.project, change?.number],
    queryFn: () => api.get<ReviewDetailView>(paths.apiReview(instance, change!.project, change!.number)),
    enabled: change !== null,
  });
  if (change === null || isStatus(review.error, 404)) return <NotFound />;
  if (review.isPending) return <Loading />;
  if (review.isError) return <ErrorBox error={review.error} />;
  return <Review view={review.data} />;
}

function Review({ view }: { view: ReviewDetailView }) {
  const { header, runs, can } = view;
  const [params] = useSearchParams();
  const selected = runs.find((r) => r.id === params.get("run")) ?? runs[0];
  const platform = platformName(header.platform);
  const sub = [
    changeRef(header.platform, header.project, header.number),
    platform,
    `${header.sourceBranch || "?"} → ${header.targetBranch || "?"}`,
    header.author ? `opened by ${header.author}` : "",
  ]
    .filter(Boolean)
    .join(" · ");
  return (
    <>
      <div className="crumbs">
        <Link to="/reviews">Reviews</Link> / <Link to={paths.repo(header.instance, header.project)}>{header.project}</Link> /{" "}
        {header.platform === "github" ? "#" : "!"}
        {header.number}
      </div>
      <div className="head">
        <div>
          <h1>
            {header.title || changeRef(header.platform, header.project, header.number)} {runs[0] && <OutcomePill outcome={runs[0].outcome} />}
          </h1>
          <div className="sub">
            {sub}
            {header.webURL && " · "}
            <ExternalLink href={header.webURL}>open in {platform} ↗</ExternalLink>
          </div>
        </div>
        {can.rerun && <RerunButtons header={header} />}
      </div>
      <div className="layout">
        <div>
          <section className="card" aria-label="Review history">
            <h2>
              Review history <span className="meta">{runs.length === 1 ? "1 run" : `${runs.length} runs`}</span>
            </h2>
            {runs.map((r) => (
              <RunItem key={r.id} run={r} selected={r.id === selected?.id} />
            ))}
          </section>
          {selected && <ThisRun id={selected.id} platform={platform} />}
        </div>
        <div>
          {selected ? (
            <RunBody key={selected.id} id={selected.id} actions={{ header, can }} />
          ) : (
            <div className="card empty">No runs yet.</div>
          )}
        </div>
      </div>
    </>
  );
}

// useReviewAction posts to one of the review's action routes; once it is
// answered, the review, its runs and the lists are read again.
// onDone gets the answer's status first, before the pages are read again (a
// finding that moves unmounts the component that pressed the button).
function useReviewAction<B extends object>(
  header: ReviewHeader,
  action: "rerun" | "dismiss" | "restore",
  onDone?: (status: string) => void,
) {
  const client = useQueryClient();
  const write = useWrite();
  return useMutation({
    mutationFn: (body: B) =>
      write<{ status: string }>("POST", paths.apiReviewAction(header.instance, header.project, header.number, action), body),
    onSuccess: (answer) => {
      onDone?.(answer.status);
      return Promise.all(["review", "run", "reviews", "overview"].map((k) => client.invalidateQueries({ queryKey: [k] })));
    },
  });
}

const rerunNote: Record<RerunStatus, string> = {
  queued: "Re-run queued.",
  "already queued": "A re-run of this change is already queued.",
  "nothing failed": "Nothing failed, so nothing was re-run.",
  "whole review": "The whole review failed, so all of it is re-run.",
};

// RerunButtons start the review again, whole or only its failed skills.
// What the server answers (a cap, a closed change) is shown as text.
function RerunButtons({ header }: { header: ReviewHeader }) {
  const rerun = useReviewAction<{ failed: boolean }>(header, "rerun");
  const note = rerun.isSuccess ? (rerunNote[rerun.data.status as RerunStatus] ?? "") : "";
  return (
    <div className="review-actions">
      <div className="actions-row">
        <button type="button" className="btn primary" disabled={rerun.isPending} onClick={() => rerun.mutate({ failed: false })}>
          Re-run review
        </button>
        <button type="button" className="btn" disabled={rerun.isPending} onClick={() => rerun.mutate({ failed: true })}>
          Re-run failed only
        </button>
      </div>
      {note && (
        <div className="meta" role="status">
          {note}
        </div>
      )}
      {rerun.isError && <ErrorBox error={rerun.error} />}
    </div>
  );
}

function RunItem({ run, selected }: { run: RunSummary; selected: boolean }) {
  const meta = [
    triggers[run.trigger] ?? run.trigger,
    ago(run.createdAt),
    run.durationSeconds !== null ? duration(run.durationSeconds) : "",
    run.outcome !== "approved" ? run.reason : "",
  ]
    .filter(Boolean)
    .join(" · ");
  return (
    <Link
      to={`?run=${encodeURIComponent(run.id)}`}
      className={selected ? "run-item sel" : "run-item"}
      aria-current={selected ? "true" : undefined}
    >
      <OutcomePill outcome={run.outcome} /> <span className="mono">{run.head.slice(0, 7)}</span>
      <div className="meta">{meta}</div>
    </Link>
  );
}

function useRun(id: string) {
  return useQuery({ queryKey: ["run", id], queryFn: () => api.get<RunDetailView>(paths.apiRun(id)) });
}

const statusTone: Record<string, Tone> = {
  success: "ok",
  failed: "bad",
  running: "run",
  neutral: "grey",
  canceled: "grey",
};

function ThisRun({ id, platform }: { id: string; platform: string }) {
  const run = useRun(id);
  return (
    <section className="card" aria-label="This run">
      <h2>This run</h2>
      <div className="facts">
        {run.isPending && <Loading />}
        {run.isError && <ErrorBox error={run.error} />}
        {run.isSuccess && <Facts run={run.data} platform={platform} />}
      </div>
    </section>
  );
}

function Facts({ run, platform }: { run: RunDetailView; platform: string }) {
  const f = run.files;
  return (
    <>
      <div>
        Status on {platform}: {run.status ? <Pill tone={statusTone[run.status] ?? "grey"}>{run.status}</Pill> : "none"}
      </div>
      <div>{run.approved ? "Approved: yes" : `Approved: no — ${run.approvalReason || "not approved"}`}</div>
      <div>
        pergent <span className="mono">{run.pergentVersion || "not recorded"}</span> · model{" "}
        <span className="mono">{run.model ?? "not recorded"}</span>
      </div>
      <div>
        Tokens: {count(run.tokens.in)} in · {count(run.tokens.out)} out
      </div>
      <div>
        Files: {f.reviewed.length} reviewed · {f.skipped.length} skipped
        {f.notReviewed.length > 0 && ` · ${f.notReviewed.length} not reviewed`}
      </div>
    </>
  );
}

const sourceText = (s: SkillView["source"]) =>
  s === "library" ? "library skill" : s === "repo" ? "repository skill" : "source not recorded";

function plural(n: number, word: string) {
  return `${n} ${word}${n === 1 ? "" : "s"}`;
}

function VerdictPill({ skill }: { skill: SkillView }) {
  if (skill.verdict === "failed") return <Pill tone="warn">Failed</Pill>;
  if (skill.verdict === "has_blocking_issues") {
    const n = skill.findings.filter((x) => x.severity === "blocking").length;
    return <Pill tone="bad">{n > 0 ? `${n} blocking` : "Blocking"}</Pill>;
  }
  return <Pill tone="ok">No blocking</Pill>;
}

type Tab = "findings" | "checks" | "files" | "logs";

// Actions are what the findings' buttons need: the review they act on and
// what the session may do. They are offered on the run the server acts on
// only (latestDecided); on any other, null.
type Actions = { header: ReviewHeader; can: Can };

// RunActions are Actions with where to report what the server answered.
type RunActions = Actions & { done: (note: string) => void };

type Located = FindingView & { skill: string };

function RunBody({ id, actions: allowed }: { id: string; actions: Actions }) {
  const run = useRun(id);
  const [tab, setTab] = useState<Tab>("findings");
  // What the last dismiss or restore answered, kept here: the finding it
  // was for moves, and its own component goes.
  const [note, setNote] = useState("");
  if (run.isPending) return <Loading />;
  if (run.isError) return <ErrorBox error={run.error} />;
  const view = run.data;
  const actions: RunActions | null = view.latestDecided ? { ...allowed, done: setNote } : null;
  // The server dismisses a fingerprint at the severity of its most severe
  // report in the run, whichever skill made it.
  const blockingFPs = new Set(
    view.skills.flatMap((s) => [...s.findings, ...s.dismissed]).filter((f) => f.severity !== "suggestion").map((f) => f.fingerprint),
  );
  const findings: Located[] = view.skills
    .flatMap((s) => s.findings.map((f) => ({ ...f, skill: s.name })))
    .sort((a, b) => Number(b.severity === "blocking") - Number(a.severity === "blocking"));
  return (
    <>
      <section className="card" aria-label="Skills">
        <h2>Skills</h2>
        {view.skills.length === 0 && (
          <div className="verdict prose">This run has no skill results: {view.approvalReason || view.run.reason || "no usable review."}</div>
        )}
        {view.skills.map((s) => (
          <div className="verdict" key={s.name}>
            <VerdictPill skill={s} />
            <div className="grow">
              <b>{s.name}</b>
              <div className="meta">
                {plural(s.findings.filter((x) => x.severity === "suggestion").length, "suggestion")}
                {s.dismissed.length > 0 && ` · ${s.dismissed.length} dismissed`} · {sourceText(s.source)}
              </div>
            </div>
          </div>
        ))}
      </section>
      <div className="card">
        <Tabs<Tab>
          inCard
          value={tab}
          onChange={setTab}
          tabs={[
            ["findings", `Findings (${findings.length})`],
            ["checks", "Checks"],
            ["files", "Files"],
            ["logs", "Logs"],
          ]}
        />
        <div role="tabpanel">
          {tab === "findings" && (
            <>
              {note && (
                <div className="note pad" role="status">
                  {note}
                </div>
              )}
              {findings.length === 0 && <div className="pad meta">No findings.</div>}
              {findings.map((f, i) => (
                <Finding key={`${f.skill}/${f.id}/${i}`} f={f} blocking={f.severity !== "suggestion" || blockingFPs.has(f.fingerprint)} actions={actions} />
              ))}
              {view.skills
                .filter((s) => s.dismissed.length > 0)
                .map((s) => (
                  <div role="group" aria-label={`Dismissed in ${s.name}`} key={s.name}>
                    <h3 className="group">
                      Dismissed in {s.name} ({s.dismissed.length})
                    </h3>
                    {s.dismissed.map((f, i) => (
                      <DismissedFinding key={`${f.id}/${i}`} f={{ ...f, skill: s.name }} actions={actions} />
                    ))}
                  </div>
                ))}
            </>
          )}
          {tab === "checks" && <Checks skills={view.skills} />}
          {tab === "files" && <Files files={view.files} />}
          {tab === "logs" && <Log id={id} />}
        </div>
      </div>
    </>
  );
}

function FindingBody({ f }: { f: Located }) {
  return (
    <>
      <Pill tone={f.severity === "blocking" ? "bad" : "grey"}>{f.severity}</Pill> <span className="t">{f.title}</span>{" "}
      <span className="mono meta">{f.id}</span>
      <div className="loc">
        {f.file}
        {f.line ? `:${f.line}` : ""} · {f.skill}
      </div>
      {f.explanation && <p className="prose">{f.explanation}</p>}
      {f.suggestedFix && (
        <>
          <div className="fix-label">Suggested fix</div>
          <pre className="fix">{f.suggestedFix}</pre>
        </>
      )}
    </>
  );
}

// Finding is a finding that stands, with the Dismiss button when the session
// may dismiss one of its severity. blocking is the severity the server will
// dismiss it at: that of the most severe report of its fingerprint. A finding
// with no fingerprint can't be named, so it can't be dismissed.
function Finding({ f, blocking, actions }: { f: Located; blocking: boolean; actions: RunActions | null }) {
  const may =
    actions !== null && f.fingerprint !== "" && (blocking ? actions.can.dismissBlocking : actions.can.dismissSuggestion);
  return (
    <div className="finding">
      <FindingBody f={f} />
      {may && <DismissControl f={f} blocking={blocking} actions={actions} />}
    </div>
  );
}

// DismissControl is the Dismiss button and the form it opens. A blocking
// finding needs a reason. The server decides in the end (it may refuse the
// author, a closed change), and its text is shown.
function DismissControl({ f, blocking, actions }: { f: Located; blocking: boolean; actions: RunActions }) {
  const [open, setOpen] = useState(false);
  const [reason, setReason] = useState("");
  const [problem, setProblem] = useState("");
  const dismiss = useReviewAction<{ fingerprint: string; reason: string }>(actions.header, "dismiss", (status) => {
    setOpen(false);
    setReason("");
    actions.done(`Finding ${status}.`);
  });
  const submit = (e: FormEvent) => {
    e.preventDefault();
    const why = reason.trim();
    if (blocking && why === "") {
      setProblem("Dismissing a blocking finding needs a reason.");
      return;
    }
    setProblem("");
    dismiss.mutate({ fingerprint: f.fingerprint, reason: why });
  };
  if (!open) {
    return (
      <div className="finding-actions">
        <button type="button" className="btn small" onClick={() => setOpen(true)}>
          Dismiss
        </button>
      </div>
    );
  }
  return (
    <form className="dismiss-form" onSubmit={submit}>
      <textarea
        aria-label="Reason"
        placeholder={blocking ? "Why is this not a problem? (required)" : "Why? (optional)"}
        rows={2}
        maxLength={500}
        value={reason}
        disabled={dismiss.isPending}
        onChange={(e) => setReason(e.target.value)}
      />
      <div className="actions-row">
        <button type="submit" className="btn small primary" disabled={dismiss.isPending}>
          Dismiss finding
        </button>
        <button type="button" className="btn small" disabled={dismiss.isPending} onClick={() => setOpen(false)}>
          Cancel
        </button>
        {problem && <span className="meta problem">{problem}</span>}
      </div>
      {dismiss.isError && <ErrorBox error={dismiss.error} />}
    </form>
  );
}

// DismissedFinding is a finding set aside: who did it and why, and Restore
// when the session may. by and reason are text people wrote.
function DismissedFinding({ f, actions }: { f: Located; actions: RunActions | null }) {
  const d = f.dismissed;
  return (
    <div className="finding">
      <FindingBody f={f} />
      {d && (
        <div className="dismissal">
          <div className="meta">
            Dismissed by {d.by} · {ago(d.at)}
          </div>
          <div className="prose">{d.reason ? `Reason: ${d.reason}` : "No reason was given."}</div>
        </div>
      )}
      {actions?.can.restore && f.fingerprint !== "" && <RestoreButton fingerprint={f.fingerprint} actions={actions} />}
    </div>
  );
}

function RestoreButton({ fingerprint, actions }: { fingerprint: string; actions: RunActions }) {
  const restore = useReviewAction<{ fingerprint: string }>(actions.header, "restore", (status) =>
    actions.done(`Finding ${status}.`),
  );
  return (
    <>
      <div className="finding-actions">
        <button type="button" className="btn small" disabled={restore.isPending} onClick={() => restore.mutate({ fingerprint })}>
          Restore
        </button>
      </div>
      {restore.isError && <ErrorBox error={restore.error} />}
    </>
  );
}

const checkTone: Record<string, Tone> = { pass: "ok", fail: "bad" };

function Checks({ skills }: { skills: SkillView[] }) {
  const withChecks = skills.filter((s) => s.checks.length > 0);
  if (withChecks.length === 0) return <div className="pad meta">No checklist was recorded for this run.</div>;
  return (
    <>
      {withChecks.map((s) => (
        <div key={s.name}>
          <h3 className="group">{s.name}</h3>
          {s.checks.map((c: CheckView, i) => (
            <div className="check" key={i}>
              <Pill tone={checkTone[c.status] ?? "grey"}>{c.status}</Pill>
              <div className="grow">
                <div className="t">{c.name}</div>
                <div className="meta">
                  {c.section} · batch {c.batch}
                </div>
                {c.detail && <div className="prose">{c.detail}</div>}
              </div>
            </div>
          ))}
        </div>
      ))}
    </>
  );
}

function Files({ files }: { files: RunDetailView["files"] }) {
  return (
    <div className="pad">
      <h3 className="group">Reviewed ({files.reviewed.length})</h3>
      <ul className="files">
        {files.reviewed.map((p) => (
          <li key={p} className="mono">
            {p}
          </li>
        ))}
      </ul>
      <h3 className="group">Skipped ({files.skipped.length})</h3>
      <ul className="files">
        {files.skipped.map((s) => (
          <li key={s.path}>
            <span className="mono">{s.path}</span> <span className="meta">{s.rule}</span>
          </li>
        ))}
      </ul>
      {files.notReviewed.length > 0 && (
        <>
          <h3 className="group">Not reviewed ({files.notReviewed.length})</h3>
          <ul className="files">
            {files.notReviewed.map((s) => (
              <li key={s.path}>
                <span className="mono">{s.path}</span> <span className="meta">{s.cause}</span>
              </li>
            ))}
          </ul>
        </>
      )}
    </div>
  );
}

function Log({ id }: { id: string }) {
  const log = useQuery({ queryKey: ["log", id], queryFn: () => api.get<LogView>(paths.apiRunLog(id)) });
  if (isStatus(log.error, 404)) return <div className="pad meta">No log was kept for this run.</div>;
  if (log.isPending) return <Loading />;
  if (log.isError) return <ErrorBox error={log.error} />;
  return <pre className="log">{log.data.text}</pre>;
}
