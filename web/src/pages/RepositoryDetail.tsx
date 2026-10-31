import { useQuery } from "@tanstack/react-query";
import { Link, useParams, useSearchParams } from "react-router";

import { api, isStatus, paths, type InputsView, type RepositoryDetailView } from "../api";
import { ErrorBox, Loading, NotFound, Tabs, platformName } from "../ui";
import AttentionRows from "./Attention";
import ReviewList from "./ReviewList";
import SettingsTab from "./RepositorySettings";

type Tab = "reviews" | "settings" | "inputs" | "setup";

// RepositoryDetail is one repository: its reviews, settings (maintainers and admins),
// resolved skills and instructions, and setup checks. The tab is in the
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
  const tabs: [Tab, string][] = [["reviews", "Reviews"]];
  if (view.role === "admin" || view.role === "maintainer") tabs.push(["settings", "Settings"]);
  tabs.push(["inputs", "Skills & instructions"], ["setup", `Setup checks (${view.attention.length})`]);
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
      <Tabs<Tab> tabs={tabs} value={tab} onChange={(t) => setSearch(t === "reviews" ? {} : { tab: t }, { replace: true })} />
      <div role="tabpanel">
        {tab === "reviews" && <ReviewList filter={{ project: repo.project, days: 90 }} label="Reviews of this repository" />}
        {tab === "settings" && <SettingsTab view={view} queryKey={queryKey} />}
        {tab === "inputs" && <Inputs instance={repo.instance} project={repo.project} />}
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
          <details className="skill-doc" key={s.name}>
            <summary className="skill">
              <b className="grow">{s.name}</b>
              <span className="src">{s.source}</span>
            </summary>
            <pre className="doc">{s.content}</pre>
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
