import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useParams, useSearchParams } from "react-router";

import { api, isStatus, paths, safeHref, type InputsView, type RepositoryDetailView, type Role, type WaitingView } from "../api";
import { useWrite } from "../layout/write";
import { QuotaLine } from "../quota";
import { ErrorBox, ExternalLink, Loading, NotFound, Tabs, platformName, skillOrigin } from "../ui";
import AttentionRows from "./Attention";
import PastDismissals from "./PastDismissals";
import Readiness from "./Readiness";
import ReviewList from "./ReviewList";
import SettingsTab from "./RepositorySettings";
import SkillResults from "./SkillResults";

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
  if (detail.isError) return <ErrorBox error={detail.error} />;

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
    <div className="narrow">
      <div className="crumbs">
        <Link to="/repos">Repositories</Link> / {repo.project}
      </div>
      <h1>{repo.project}</h1>
      <div className="sub spaced">
        {platformName(repo.platform)} · {repo.instance} · {repo.source === "config" ? "managed by config" : "switched on in the dashboard"}
      </div>
      {view.role === "admin" && view.quota && (
        <div className="quota" role="group" aria-label="Token usage this month">
          <QuotaLine label="This repository" ariaLabel="Repository token quota" use={view.quota.repository} />
          <QuotaLine label="Its instance" ariaLabel="Instance token quota" use={view.quota.instance} />
        </div>
      )}
      {view.waiting.length > 0 && (
        <Waiting
          instance={repo.instance}
          project={repo.project}
          mark={repo.platform === "github" ? "#" : "!"}
          items={view.waiting}
          role={view.role}
          queryKey={queryKey}
        />
      )}
      <Tabs<Tab> tabs={tabs} value={tab} onChange={(t) => setSearch(t === "reviews" ? {} : { tab: t }, { replace: true })} />
      <div role="tabpanel">
        {tab === "reviews" && <ReviewList filter={{ project: repo.project, days: 90 }} label="Reviews of this repository" />}
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
        {tab === "inputs" && <Inputs instance={repo.instance} project={repo.project} />}
        {tab === "readiness" && <Readiness instance={repo.instance} project={repo.project} />}
        {tab === "setup" && (
          <section className="card" aria-label="Setup checks">
            <h2>Setup checks</h2>
            <AttentionRows items={view.attention} platform={repo.platform} linkRepos={false} />
          </section>
        )}
      </div>
    </div>
  );
}

// Waiting is the changes whose review waits for someone to start it: authors
// the repository doesn't trust (a maintainer or an admin starts those) and
// changes held by a used-up token quota (only an admin starts those).
function Waiting({
  instance,
  project,
  mark,
  items,
  role,
  queryKey,
}: {
  instance: string;
  project: string;
  // mark is how the platform writes a change's number: ! on GitLab, # on GitHub.
  mark: string;
  items: WaitingView[];
  role: Role;
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
    <section className="card" aria-label="Waiting for a start">
      <h2>Waiting for a start</h2>
      <p className="pad meta">
        Nobody reviews these changes until someone starts the review.
      </p>
      {items.map((w) => (
        <div className="row" key={w.number}>
          <div className="grow">
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
            {w.title && <span className="meta"> · {w.title}</span>}
            {w.reason === "trust_check_failed" && <div className="meta">Couldn't check the author's access.</div>}
            {w.reason === "quota_used_up" && <div className="meta">Monthly token quota used up.</div>}
          </div>
          {(role === "admin" || (role === "maintainer" && w.reason !== "quota_used_up")) && (
            <button
              type="button"
              className="btn small"
              aria-label={`Start review of ${mark}${w.number}`}
              disabled={start.isPending}
              onClick={() => start.mutate(w)}
            >
              Start review
            </button>
          )}
        </div>
      ))}
      {start.isError && <ErrorBox error={start.error} />}
    </section>
  );
}

function Inputs({ instance, project }: { instance: string; project: string }) {
  const inputs = useQuery({
    queryKey: ["inputs", instance, project],
    queryFn: () => api.get<InputsView>(paths.apiRepoInputs(instance, project)),
  });
  if (inputs.isPending) return <Loading />;
  if (inputs.isError) return <ErrorBox error={inputs.error} />;
  const v = inputs.data;
  return (
    <>
      <p className="sub">
        Read from the default branch <span className="mono">{v.branch}</span>, as a review would get them. Kept for 5 minutes.
      </p>
      <section className="card" aria-label="Skills">
        <h2>Skills</h2>
        {v.skills.length === 0 && <div className="row empty-row">No skills.</div>}
        {v.skills.map((s) => (
          <details className="skill-doc" key={`${s.name}/${s.folder}`}>
            <summary className="skill">
              <b className="grow">{s.name}</b>
              {s.problem && <span className="problem">{s.problem}</span>}
              {s.source !== "" && <span className="src">{skillOrigin(s.source, s.version)}</span>}
            </summary>
            {!s.problem && <pre className="doc">{s.content}</pre>}
            {safeHref(s.url) && (
              <div className="pad meta">
                <ExternalLink href={s.url}>View file</ExternalLink>
              </div>
            )}
          </details>
        ))}
      </section>
      <section className="card" aria-label="Instructions">
        <h2>Instructions</h2>
        {v.instructions ? <pre className="doc">{v.instructions}</pre> : <div className="row empty-row">No instructions.</div>}
      </section>
    </>
  );
}
