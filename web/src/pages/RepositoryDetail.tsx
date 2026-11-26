import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { Link, useParams, useSearchParams } from "react-router";

import { ChevronRight } from "lucide-react";

import { Alert } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardHeader, CardTitle } from "@/components/ui/card";

import { api, isStatus, paths, safeHref, type InputsView, type MultiReviewView, type RepositoryDetailView, type Role, type WaitingView } from "../api";
import { useWrite } from "../layout/write";
import { QuotaLine } from "../quota";
import { ErrorBox, ExternalLink, Loading, NotFound, PageHeader, Tabs, platformName, skillOrigin } from "../ui";
import AttentionRows from "./Attention";
import PastDismissals from "./PastDismissals";
import Readiness from "./Readiness";
import RepoModelChain from "./RepoModelChain";
import ReviewList from "./ReviewList";
import SettingsTab from "./RepositorySettings";
import SkillResults from "./SkillResults";
import Markdown from "../MarkdownLazy";

// cardClass is a card of rows; headClass is its title bar.
const cardClass = "mb-4 gap-0 overflow-hidden py-0";
const headClass = "border-b py-3 [.border-b]:pb-3";

// rowClass is a row of a card; muted is a line of muted text in one.
const rowClass = "flex items-center gap-3 border-b px-4 py-3 text-sm last:border-b-0";
const muted = "text-muted-foreground border-b px-4 py-3 text-sm last:border-b-0";

type Tab = "reviews" | "skill-results" | "settings" | "past-dismissals" | "inputs" | "readiness" | "setup";

// RepositoryDetail is one repository: its reviews, skill results, settings and past dismissals
// (maintainers and admins), resolved skills and instructions, readiness scan, and setup checks. The tab is in the
// address (?tab=), so the project path, which has slashes, stays whole.
export default function RepositoryDetail() {
  const params = useParams();
  const instance = params.instance ?? "";
  const project = params["*"] ?? "";
  const queryKey = ["repo", instance, project];
  const detail = useQuery({
    queryKey,
    queryFn: () => api.get<RepositoryDetailView>(paths.apiRepo(instance, project)),
    enabled: project !== "",
  });
  const [search, setSearch] = useSearchParams();
  if (project === "" || isStatus(detail.error, 404)) return <NotFound />;
  if (detail.isPending) return <Loading />;
  if (detail.isError) return <ErrorBox error={detail.error} onRetry={() => void detail.refetch()} />;

  const view = detail.data;
  const { repo } = view;
  const tabs: [Tab, string][] = [
    ["reviews", "Reviews"],
    ["skill-results", "Skill results"],
  ];
  if (view.role === "admin" || view.role === "maintainer") tabs.push(["settings", "Settings"], ["past-dismissals", "Past dismissals"]);
  tabs.push(["inputs", "Skills & instructions"], ["readiness", "Readiness"], ["setup", `Setup checks (${view.attention.length})`]);
  const asked = search.get("tab") as Tab | null;
  const tab: Tab = tabs.some(([t]) => t === asked) ? (asked as Tab) : "reviews";

  return (
    <div className="max-w-[1100px]">
      <div className="text-muted-foreground mb-1.5 text-sm">
        <Link className="hover:text-foreground hover:underline" to="/repos">
          Repositories
        </Link>{" "}
        / {repo.project}
      </div>
      {/* One line of facts, so the tabs start near the top. */}
      <PageHeader
        className="mb-4"
        title={repo.project}
        description={(
          [
            ["platform", platformName(repo.platform)],
            ["instance", repo.instance],
            ["state", repo.source === "config" ? "managed by config" : repo.paused ? "paused in the dashboard" : "switched on in the dashboard"],
            [
              "owner",
              view.ownerTeam ? (
                <>
                  Owner team:{" "}
                  {view.ownerTeam.open ? (
                    <Link className="text-primary hover:underline" to={paths.team(view.ownerTeam.id)}>
                      {view.ownerTeam.name}
                    </Link>
                  ) : (
                    view.ownerTeam.name
                  )}
                </>
              ) : null,
            ],
            ["multi", multiReviewText(view.multiReview)],
            ["gate", view.settings.blockingStatus.value === "neutral" ? "Merge gate: off (blocking status is neutral)" : null],
          ] as [string, ReactNode][]
        )
          .filter(([, fact]) => fact !== null)
          .map(([key, fact], i) => (
            <span key={key}>
              {i > 0 && " · "}
              <span>{fact}</span>
            </span>
          ))}
      />
      <OwnerActions view={view} queryKey={queryKey} />
      {repo.paused && <Paused instance={repo.instance} project={repo.project} canResume={view.actions.resume} queryKey={queryKey} />}
      {view.role === "admin" && view.quota && (
        <Card className="mb-4 gap-0 px-4 py-2.5" role="group" aria-label="Token usage this month">
          <QuotaLine label="This repository" ariaLabel="Repository token quota" use={view.quota.repository} />
          <QuotaLine label="Its instance" ariaLabel="Instance token quota" use={view.quota.instance} />
        </Card>
      )}
      {view.waiting.length > 0 && (
        <Waiting
          instance={repo.instance}
          project={repo.project}
          mark={repo.platform === "github" ? "#" : "!"}
          items={view.waiting}
          role={view.role}
          paused={repo.paused}
          queryKey={queryKey}
        />
      )}
      <Tabs<Tab> tabs={tabs} value={tab} onChange={(t) => setSearch(t === "reviews" ? {} : { tab: t })} />
      <div role="tabpanel">
        {tab === "reviews" && <ReviewList filter={{ project: repo.project, days: 90 }} label="Reviews of this repository" inRepo />}
        {tab === "skill-results" && <SkillResults instance={repo.instance} project={repo.project} />}
        {tab === "settings" && <SettingsTab view={view} queryKey={queryKey} />}
        {tab === "past-dismissals" && (
          <PastDismissals
            instance={repo.instance}
            project={repo.project}
            platform={repo.platform}
            learning={view.settings.learnFromDismissals.value}
          />
        )}
        {tab === "inputs" && <Inputs instance={repo.instance} project={repo.project} builtInsOff={view.builtInRulesOff} />}
        {/* A viewer has no Settings tab, where the model chain is: it is
            shown here, with what else a review would get. */}
        {tab === "inputs" && view.role === "viewer" && <RepoModelChain instance={repo.instance} project={repo.project} />}
        {tab === "readiness" && <Readiness instance={repo.instance} project={repo.project} paused={repo.paused} />}
        {tab === "setup" && (
          <Card role="region" aria-label="Setup checks" className={cardClass}>
            <CardHeader className={headClass}>
              <CardTitle>Setup checks</CardTitle>
            </CardHeader>
            <AttentionRows items={view.attention} />
          </Card>
        )}
      </div>
    </div>
  );
}

// OwnerActions are Pause and Release ownership, each shown when the server's
// flag for the session is true. Release asks first.
function OwnerActions({ view, queryKey }: { view: RepositoryDetailView; queryKey: unknown[] }) {
  const client = useQueryClient();
  const write = useWrite();
  const { repo, ownerTeam } = view;
  const done = () => {
    client.invalidateQueries({ queryKey });
    client.invalidateQueries({ queryKey: ["repositories"] });
    client.invalidateQueries({ queryKey: ["teams"] });
    client.invalidateQueries({ queryKey: ["my-teams"] });
  };
  const pause = useMutation({
    mutationFn: () => write<RepositoryDetailView>("PUT", paths.apiAdminRepo(repo.instance, repo.project), { enabled: false }),
    onSuccess: done,
  });
  const release = useMutation({
    mutationFn: () => write<RepositoryDetailView>("POST", paths.apiRepoRelease(repo.instance, repo.project), {}),
    onSuccess: done,
  });
  const ask = () => {
    const team = ownerTeam?.name ?? "The owner team";
    if (
      window.confirm(
        `Release ownership of ${repo.project}? ${team} keeps access, and the repository has no owner team until a team claims it.`,
      )
    )
      release.mutate();
  };
  if (!view.actions.pause && !view.actions.release) return null;
  return (
    <div className="mb-4 flex flex-wrap items-center gap-2">
      {view.actions.pause && (
        <Button type="button" variant="outline" size="sm" disabled={pause.isPending} onClick={() => pause.mutate()}>
          Pause
        </Button>
      )}
      {view.actions.release && (
        <Button type="button" variant="outline" size="sm" disabled={release.isPending} onClick={ask}>
          Release ownership
        </Button>
      )}
      {pause.isError && <ErrorBox error={pause.error} />}
      {release.isError && <ErrorBox error={release.error} />}
    </div>
  );
}

// Paused says the repository is paused; a session the server lets resume it
// does from here, as the switch on Repositories does.
function Paused({ instance, project, canResume, queryKey }: { instance: string; project: string; canResume: boolean; queryKey: unknown[] }) {
  const client = useQueryClient();
  const write = useWrite();
  const resume = useMutation({
    mutationFn: () => write<RepositoryDetailView>("PUT", paths.apiAdminRepo(instance, project), { enabled: true }),
    onSuccess: () => {
      client.invalidateQueries({ queryKey });
      client.invalidateQueries({ queryKey: ["repositories"] });
    },
  });
  return (
    <Alert variant="warn" className="mb-4 flex flex-wrap items-center gap-3" role="note" aria-label="Paused">
      <span className="prose min-w-0 grow">
        <strong>Paused:</strong> pergent doesn't review this repository, and its settings are kept.
        {!canResume && " An admin can switch it on again."}
      </span>
      {canResume && (
        <Button
          type="button"
          variant="outline"
          size="sm"
          className="text-foreground"
          disabled={resume.isPending}
          onClick={() => resume.mutate()}
        >
          Resume
        </Button>
      )}
      {resume.isError && (
        <div className="basis-full">
          <ErrorBox error={resume.error} />
        </div>
      )}
    </Alert>
  );
}

// multiReviewText is the multi review line: on with the count of reviewers
// and the judge's name, or off.
const multiReviewText = (m: MultiReviewView) =>
  m.on ? `Multi review: on (${m.reviewers} reviewers, ${m.judge ? `judge ${m.judge}` : "no judge"})` : "Multi review: off";

// heldForTokens says a change waits because the tokens ran out: a quota, or
// a model or team limit with it. Only an admin starts one.
const heldForTokens = (w: WaitingView) => w.reason === "quota_used_up" || w.reason === "limit_used_up";

// Waiting is the changes whose review waits for someone to start it: authors
// the repository doesn't trust (a maintainer or an admin starts those) and
// changes held by a used-up token quota, model limit or team limit (only an
// admin starts those).
function Waiting({
  instance,
  project,
  mark,
  items,
  role,
  paused,
  queryKey,
}: {
  instance: string;
  project: string;
  // mark is how the platform writes a change's number: ! on GitLab, # on GitHub.
  mark: string;
  items: WaitingView[];
  role: Role;
  // paused hides Start review: nothing starts while the repository is paused.
  paused: boolean;
  queryKey: unknown[];
}) {
  const client = useQueryClient();
  const write = useWrite();
  const start = useMutation({
    // The head shown is sent back: a change pushed since doesn't start.
    mutationFn: (w: WaitingView) => write<{ status: string }>("POST", paths.apiReviewAction(instance, project, w.number, "start"), { head: w.head }),
    onSuccess: () => client.invalidateQueries({ queryKey }),
  });
  return (
    <Card role="region" aria-label="Waiting for a start" className={cardClass}>
      <CardHeader className={headClass}>
        <CardTitle>Waiting for a start</CardTitle>
      </CardHeader>
      <p className={muted}>Nobody reviews these changes until someone starts the review.</p>
      {items.map((w) => (
        <div className={rowClass} key={w.number}>
          <div className="[&_a]:text-primary min-w-0 grow [&_a]:font-medium [&_a]:hover:underline">
            {safeHref(w.webUrl) ? (
              <ExternalLink href={w.webUrl}>
                {mark}
                {w.number}
              </ExternalLink>
            ) : (
              <span>
                {mark}
                {w.number}
              </span>
            )}{" "}
            by {w.author}
            {w.title && <span className="text-muted-foreground"> · {w.title}</span>}
            {w.reason === "trust_check_failed" && <div className="text-muted-foreground text-xs">Couldn't check the author's access.</div>}
            {w.reason === "quota_used_up" && <div className="text-muted-foreground text-xs">Monthly token quota used up.</div>}
            {w.reason === "limit_used_up" && <div className="text-muted-foreground text-xs">No model in the chain has room this month.</div>}
          </div>
          {!paused && (role === "admin" || (role === "maintainer" && !heldForTokens(w))) && (
            <Button
              type="button"
              variant="outline"
              size="sm"
              aria-label={`Start review of ${mark}${w.number}`}
              disabled={start.isPending}
              onClick={() => start.mutate(w)}
            >
              Start review
            </Button>
          )}
        </div>
      ))}
      {start.isError && (
        <div className="px-4 py-3">
          <ErrorBox error={start.error} />
        </div>
      )}
    </Card>
  );
}

function Inputs({ instance, project, builtInsOff }: { instance: string; project: string; builtInsOff: string }) {
  const inputs = useQuery({
    queryKey: ["inputs", instance, project],
    queryFn: () => api.get<InputsView>(paths.apiRepoInputs(instance, project)),
  });
  if (inputs.isPending) return <Loading />;
  if (inputs.isError) return <ErrorBox error={inputs.error} />;
  const v = inputs.data;
  return (
    <>
      <p className="text-muted-foreground mb-4 text-sm">
        Read from the default branch <span className="font-mono text-xs">{v.branch}</span>, as a review would get them. Kept for 5
        minutes.
      </p>
      <Card role="region" aria-label="Skills" className={cardClass}>
        <CardHeader className={headClass}>
          <CardTitle>Skills</CardTitle>
        </CardHeader>
        {v.skills.length === 0 && <div className={muted}>No skills.</div>}
        {v.skills.map((s) => (
          <details className={docClass} key={`${s.name}/${s.folder}`}>
            <Summary>
              <b className="min-w-0 grow font-semibold">{s.name}</b>
              {s.problem && <span className="text-bad text-xs">{s.problem}</span>}
              {s.source !== "" && <Src>{skillOrigin(s.source, s.version)}</Src>}
            </Summary>
            {!s.problem && <Markdown text={s.content} />}
            {safeHref(s.url) && (
              <div className="text-muted-foreground [&_a]:text-primary px-4 pb-3 text-sm [&_a]:hover:underline">
                <ExternalLink href={s.url}>View file</ExternalLink>
              </div>
            )}
          </details>
        ))}
      </Card>
      <Card role="region" aria-label="Instructions" className={cardClass}>
        <CardHeader className={headClass}>
          <CardTitle>Instructions</CardTitle>
        </CardHeader>
        {v.instructions ? (
          <div className="pt-3">
            <Markdown text={v.instructions} />
          </div>
        ) : (
          <div className={muted}>No instructions.</div>
        )}
      </Card>
      {v.rules && <RuleList rules={v.rules} builtInsOff={builtInsOff} />}
    </>
  );
}

// RuleList is the path rule list a review would get, in match order: the
// repository's .pergent/rules.yaml, then its rule sets, then the enabled
// built-in rules, each a row that opens to its text. The first entry that
// matches a file gives it its rule. builtInsOff is why reviews leave the
// built-in rules out for now ("" when they don't).
function RuleList({ rules, builtInsOff }: { rules: InputsView["rules"]; builtInsOff: string }) {
  const fromRepo = rules.entries.filter((e) => e.source === "repo").length;
  const fromSets = rules.entries.length - fromRepo;
  const builtIns = rules.builtInEntries;
  return (
    <Card role="region" aria-label="Rules" className={cardClass}>
      <CardHeader className={headClass}>
        <CardTitle>Rules</CardTitle>
      </CardHeader>
      <div className={muted}>
        {fromRepo} repository · {fromSets} rule {fromSets === 1 ? "set" : "sets"} · {builtIns.length} built-in. The first entry
        that matches a file gives it its rule.
      </div>
      {rules.problem && (
        <div className={rowClass}>
          <span className="problem text-bad text-sm">{rules.problem}</span>
        </div>
      )}
      {builtInsOff !== "" && builtIns.length > 0 && (
        <div className="problem text-bad border-b px-4 py-3 text-sm last:border-b-0">{builtInsOff}</div>
      )}
      {!rules.problem && rules.entries.length === 0 && builtIns.length === 0 && <div className={muted}>No rules.</div>}
      {rules.entries.map((e, i) => (
        <details className={docClass} key={`${i}|${e.source}|${e.path}`} data-rule={`${e.source}|${e.path}`}>
          <Summary>
            <b className="min-w-0 grow font-mono text-xs font-semibold">{e.path}</b>
            {e.off && <span className="text-muted-foreground text-xs">off: no rule</span>}
            <Src>{e.source}</Src>
          </Summary>
          {!e.off && <Markdown text={e.rule} />}
        </details>
      ))}
      {builtIns.map((b) => (
        <details className={docClass} key={`built-in|${b.name}`} data-rule={`built-in|${b.name}`}>
          <Summary>
            <b className="font-semibold">{b.name}</b>
            <span className="flex min-w-0 grow flex-wrap gap-x-2 gap-y-1">
              {b.paths.map((p) => (
                <code key={p} className="text-muted-foreground font-mono text-xs">
                  {p}
                </code>
              ))}
            </span>
            <Src>built-in</Src>
          </Summary>
          <Markdown text={b.rule} />
        </details>
      ))}
    </Card>
  );
}

// docClass is a row that opens to a document: a skill's or a rule's text.
// "skill-doc" stays as a name the tests find the rows by.
const docClass = "skill-doc group border-b last:border-b-0";

// Summary is the line of a docClass row, with an arrow that turns when it is
// open. "skill" stays as a name the tests find the line by.
function Summary({ children }: { children: ReactNode }) {
  return (
    <summary className="skill hover:bg-muted/50 flex cursor-pointer list-none items-center gap-2.5 px-4 py-2.5 text-sm before:content-none [&::-webkit-details-marker]:hidden">
      <ChevronRight className="text-muted-foreground size-4 shrink-0 transition-transform group-open:rotate-90" aria-hidden="true" />
      {children}
    </summary>
  );
}

// Src is where a skill or rule comes from, as a quiet badge. "src" stays as
// a name the tests read.
function Src({ children }: { children: ReactNode }) {
  return (
    <Badge variant="outline" className="src text-muted-foreground font-normal">
      {children}
    </Badge>
  );
}
