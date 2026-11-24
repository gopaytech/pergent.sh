import { keepPreviousData, useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState, type ReactNode } from "react";
import { Link, useNavigate } from "react-router";

import {
  api,
  paths,
  skillBudget,
  type BuiltInRule,
  type CIJobNamesView,
  type CIJobs,
  type DetectedSkill,
  type InputsView,
  type LibrarySkill,
  type RuleSet,
  type LinkableView,
  type Link as RepoLink,
  type RepoSettings,
  type RepoTeamsView,
  type RepositoryDetailView,
  type SettingsUpdate,
  type TrustedAuthor,
  type TrustMode,
} from "../api";
import BuiltInRulePicker from "../BuiltInRulePicker";
import CIJobsEditor, { tidyCIJobs } from "../CIJobsEditor";
import CommentLayoutEditor from "../CommentLayoutEditor";
import LinkedReposEditor, { tidyLinks, type LinkableState } from "../LinkedReposEditor";
import { useWrite } from "../layout/write";
import { parseQuota, quotaProblem, quotaText } from "../quota";
import RuleSetPicker from "../RuleSetPicker";
import { blockingHelp, blockingLabel, failureHelp, failureLabel, notUsedNote } from "../statusSettings";
import { ErrorBox, Loading, Switch, bytesText } from "../ui";
import RepoModelChain from "./RepoModelChain";

// SettingsTab edits a repository's settings (maintainers and admins only;
// the page hides the tab from others). A config-managed repository's are
// shown disabled. queryKey is the page's own: the address may spell the
// project differently from the server.
export default function SettingsTab({ view, queryKey }: { view: RepositoryDetailView; queryKey: unknown[] }) {
  const client = useQueryClient();
  const write = useWrite();
  const { repo } = view;
  const library = useQuery({ queryKey: ["skills"], queryFn: () => api.get<LibrarySkill[]>("/api/skills") });
  // The rule library, to choose and order rule sets from; the page doesn't
  // wait for it.
  const ruleLibrary = useQuery({ queryKey: ["rule-sets"], queryFn: () => api.get<RuleSet[]>("/api/rule-sets") });
  // pergent's built-in rules, to choose from; the page doesn't wait for them.
  const builtIns = useQuery({ queryKey: ["built-in-rules"], queryFn: () => api.get<BuiltInRule[]>("/api/built-in-rules") });
  // The skills found in the repository's folders, from the default branch:
  // the same read the Skills & instructions tab shows.
  const inputs = useQuery({
    queryKey: ["inputs", repo.instance, repo.project],
    queryFn: () => api.get<InputsView>(paths.apiRepoInputs(repo.instance, repo.project)),
  });
  const [note, setNote] = useState("");
  // Linked repositories are an admin's, on a GitHub repository the dashboard
  // sets up: only then is the list of repositories to link asked for.
  const canSetLinks = view.role === "admin";
  const [search, setSearch] = useState("");
  const linkable = useQuery({
    queryKey: ["linkable", repo.instance, repo.project, search],
    queryFn: () => api.get<LinkableView>(paths.apiLinkable(repo.instance, repo.project, search)),
    enabled: canSetLinks && repo.platform === "github" && repo.source !== "config",
    placeholderData: keepPreviousData,
  });

  // The job names the platform shows in the repository's recent runs, to
  // suggest in the CI jobs card. Not asked for a repository the config sets up
  // (nothing there is edited). A failed read gives no names and no warning.
  const ciNames = useQuery({
    queryKey: ["ci-job-names", repo.instance, repo.project],
    queryFn: () => api.get<CIJobNamesView>(paths.apiRepoCIJobNames(repo.instance, repo.project)),
    enabled: repo.source !== "config",
    retry: false,
  });

  const done = (detail: RepositoryDetailView, message: string) => {
    client.setQueryData(queryKey, detail);
    client.invalidateQueries({ queryKey: ["repositories"] });
    // The server dropped its kept read of the repository's skills.
    client.invalidateQueries({ queryKey: ["inputs", repo.instance, repo.project] });
    // What can be linked depends on the links now.
    client.invalidateQueries({ queryKey: ["linkable", repo.instance, repo.project] });
    setNote(message);
  };
  const save = useMutation({
    mutationFn: (body: SettingsUpdate) =>
      write<RepositoryDetailView>("PUT", paths.apiRepoSettings(repo.instance, repo.project), body),
    onMutate: () => setNote(""),
    onSuccess: (d) => done(d, "Saved."),
  });
  const reset = useMutation({
    mutationFn: () => write<RepositoryDetailView>("POST", paths.apiRepoReset(repo.instance, repo.project), {}),
    onMutate: () => setNote(""),
    onSuccess: (d) => done(d, "Every setting is back to its default."),
  });

  if (library.isPending) return <Loading />;
  if (library.isError) return <ErrorBox error={library.error} />;
  const locked = repo.source === "config";
  const busy = save.isPending || reset.isPending;
  return (
    <>
      <Form
        // A new answer from the server starts the form again from it.
        key={JSON.stringify(view.settings)}
        settings={view.settings}
        library={library.data}
        ruleLibrary={ruleLibrary.isSuccess ? ruleLibrary.data : null}
        builtIns={builtIns.isSuccess ? builtIns.data : builtIns.isError ? "error" : "loading"}
        builtInsOff={view.builtInRulesOff}
        detected={
          inputs.isPending
            ? { status: "loading" }
            : inputs.isError
              ? { status: "error", error: inputs.error }
              : { status: "ok", items: inputs.data.detected }
        }
        attention={view.attention.map((a) => a.message)}
        locked={locked}
        // Only an admin of the repository changes its re-run caps.
        canSetCaps={view.role === "admin"}
        canSetLinks={canSetLinks}
        // Who is trusted is an admin's to say too.
        canSetTrust={view.role === "admin"}
        // So is the token quota.
        canSetQuota={view.role === "admin"}
        // And the two status settings.
        canSetStatus={view.role === "admin"}
        instanceTrust={view.instanceTrustedAuthorsOnly}
        platform={repo.platform}
        ciNames={ciNames.isSuccess ? ciNames.data.names : null}
        linkable={
          linkable.isError
            ? { status: "error", error: linkable.error }
            : linkable.data !== undefined
              ? { status: "ok", view: linkable.data }
              : // Not asked for (the config sets the repository up): nothing to pick from.
                linkable.fetchStatus === "idle"
                ? { status: "ok", view: { items: [], truncated: false } }
                : { status: "loading" }
        }
        onSearch={setSearch}
        busy={busy}
        onSave={(body) => save.mutate(body)}
        onReset={() => {
          // Only an admin's reset covers the re-run caps.
          const caps =
            view.role === "admin"
              ? ""
              : " The re-run caps stay as they are; only an admin of this repository resets them. So do the linked repositories, who is trusted, the token quota and the status settings.";
          if (window.confirm(`Put every setting of this repository back to its default?${caps} The model chain stays as it is.`)) reset.mutate();
        }}
      />
      {note && (
        <div className="note" role="status">
          {note}
        </div>
      )}
      {save.isError && <ErrorBox error={save.error} />}
      {reset.isError && <ErrorBox error={reset.error} />}
      {/* The model chain is not a setting: it has its own routes, and the
          config does not set it, so it is there for every repository. */}
      <RepoModelChain instance={repo.instance} project={repo.project} />
      {/* Which teams cover a repository is an admin's to say, also for a
          repository the config sets up: it is not one of its settings. */}
      {view.role === "admin" && <RepoTeams instance={repo.instance} project={repo.project} />}
      {/* Last, so nothing else on the tab sits under it. Pausing is the
          switch on Repositories; the config removes its own repositories. */}
      {view.role === "admin" && repo.source === "dashboard" && <RemoveRepository instance={repo.instance} project={repo.project} />}
    </>
  );
}

// RemoveRepository deletes a repository from pergent for good, after a
// confirm, and goes back to the list.
function RemoveRepository({ instance, project }: { instance: string; project: string }) {
  const client = useQueryClient();
  const write = useWrite();
  const navigate = useNavigate();
  const remove = useMutation({
    mutationFn: () => write<RepositoryDetailView>("DELETE", paths.apiRepo(instance, project)),
    onSuccess: () => {
      client.invalidateQueries({ queryKey: ["repositories"] });
      navigate("/repos");
    },
  });
  const ask = () => {
    if (
      window.confirm(
        `Remove ${project} from pergent? Its settings and team links are deleted, and its webhook and clone credentials are removed. This can't be undone.`,
      )
    )
      remove.mutate();
  };
  return (
    <section className="card pad stack" aria-label="Remove repository">
      <h2>Remove repository</h2>
      <div className="meta">
        Its settings and team links are deleted, and its webhook and clone credentials are removed. This cannot be undone.
      </div>
      {remove.isError && <ErrorBox error={remove.error} />}
      <div className="actions-row">
        <button type="button" className="btn" disabled={remove.isPending} onClick={ask}>
          Remove repository
        </button>
      </div>
    </section>
  );
}

// RepoTeams is, for an admin, the teams that list this repository: the same
// data as the Teams page, seen from the repository. A team that covers the
// whole instance is shown as such and left alone here.
function RepoTeams({ instance, project }: { instance: string; project: string }) {
  const key = ["repo-teams", instance, project];
  // Kept here: a saved answer starts the form again, which would lose it.
  const [saved, setSaved] = useState(false);
  const teams = useQuery({ queryKey: key, queryFn: () => api.get<RepoTeamsView>(paths.apiRepoTeams(instance, project)) });
  return (
    <section className="card" aria-label="Teams">
      <h2>
        <span>Teams</span>
      </h2>
      {teams.isPending && <Loading />}
      {teams.isError && <ErrorBox error={teams.error} />}
      {teams.isSuccess &&
        (teams.data.teams.length === 0 ? (
          <div className="pad meta">
            No team yet. Create one on <Link to="/admin/teams">Teams</Link>.
          </div>
        ) : (
          // A new answer from the server starts the list again from it.
          <RepoTeamsForm
            key={JSON.stringify(teams.data)}
            data={teams.data}
            instance={instance}
            project={project}
            queryKey={key}
            saved={saved}
            setSaved={setSaved}
          />
        ))}
    </section>
  );
}

function RepoTeamsForm({
  data,
  instance,
  project,
  queryKey,
  saved,
  setSaved,
}: {
  data: RepoTeamsView;
  instance: string;
  project: string;
  queryKey: unknown[];
  saved: boolean;
  setSaved: (saved: boolean) => void;
}) {
  const client = useQueryClient();
  const write = useWrite();
  const [ticked, setTicked] = useState(() => new Set(data.teams.filter((t) => t.direct).map((t) => t.id)));
  const save = useMutation({
    mutationFn: (ids: number[]) => write<RepoTeamsView>("PUT", paths.apiRepoTeams(instance, project), { teams: ids }),
    onMutate: () => setSaved(false),
    onSuccess: (d) => {
      client.setQueryData(queryKey, d);
      // The Teams page and the Users page's team column read the same rows.
      client.invalidateQueries({ queryKey: ["teams"] });
      client.invalidateQueries({ queryKey: ["users"] });
      setSaved(true);
    },
  });
  const toggle = (id: number, on: boolean) => {
    const next = new Set(ticked);
    if (on) next.add(id);
    else next.delete(id);
    setTicked(next);
    setSaved(false);
  };
  return (
    <form
      className="pad stack"
      onSubmit={(e) => {
        e.preventDefault();
        save.mutate([...ticked].sort((a, b) => a - b));
      }}
    >
      {data.teams.map((t) => (
        <label className="check" key={t.id}>
          <input type="checkbox" checked={ticked.has(t.id)} onChange={(e) => toggle(t.id, e.target.checked)} />
          <span>
            {t.name} <span className="meta">{t.members === 1 ? "1 member" : `${t.members} members`}</span>
            {t.wholeInstance && <span className="meta"> · covers every repository of {instance}</span>}
          </span>
        </label>
      ))}
      <div className="meta">A team covers the repositories it lists, or all of an instance. Edit a team's members on Teams.</div>
      {save.isError && <ErrorBox error={save.error} />}
      {saved && (
        <div className="note" role="status">
          Saved.
        </div>
      )}
      <div className="actions-row">
        <button type="submit" className="btn primary" disabled={save.isPending}>
          Save teams
        </button>
      </div>
    </form>
  );
}

type Draft = {
  approve: boolean;
  reviewDrafts: boolean;
  inlineFindings: boolean;
  commandsAuthorAndMaintainersOnly: boolean;
  incrementalReviews: boolean;
  learnFromDismissals: boolean;
  quietReviews: boolean;
  checkDeleted: boolean;
  // The CI jobs as typed (the editor keeps blanks while someone types).
  ciJobs: CIJobs;
  // Admins only: the impact check across linked repositories, and the links
  // (blank lines in the watched paths are kept while typing).
  linkedReposEnabled: boolean;
  linkedRepos: RepoLink[];
  // null is the default layout.
  commentLayout: string[] | null;
  failureStatus: string;
  blockingStatus: string;
  // mode "default" is a null skills setting: the default list.
  skillsMode: "default" | "choose";
  skills: string[];
  repoSkills: string[];
  // mode "default" is a null ruleSets setting: the default list.
  ruleSetsMode: "default" | "choose";
  ruleSets: string[];
  // The built-in rules chosen, by name; [] is none.
  builtInRules: string[];
  jobTimeoutSeconds: string;
  maxTurns: string;
  jobMemory: string;
  rerunLimitPerMR: string;
  rerunLimitPerRepo: string;
  // "" follows the instance.
  trustedAuthorsOnly: "" | "on" | "off";
  trustedAuthors: TrustedAuthor[];
  // The quota as typed; "" is none of its own.
  monthlyTokenQuota: string;
};

// trustModeWords is a trusted-authors mode in words.
const trustModeWords = (m: TrustMode) => (m === "follow_visibility" ? "follow visibility" : m);

const stepsProblem = "Max steps is a whole number from 5 to 200.";
const capProblem = "The re-run caps are whole numbers from 1 to 1000.";

// capBody is the value to send for a re-run cap field that changed: a whole
// number from 1 to 1000, or null (empty: the default); undefined when the
// text is neither.
function capBody(text: string): number | null | undefined {
  const t = text.trim();
  if (t === "") return null;
  if (!/^\d+$/.test(t)) return undefined;
  const n = Number(t);
  return n >= 1 && n <= 1000 ? n : undefined;
}

// sameLayout is whether two layouts are the same list in the same order, or
// both the default (null).
function sameLayout(a: string[] | null, b: string[] | null) {
  return a === null || b === null ? a === b : a.length === b.length && a.every((x, i) => x === b[i]);
}

function sameSet(a: string[], b: string[]) {
  return a.length === b.length && a.every((x) => b.includes(x));
}

// DetectedState is what the read of the repository's skill folders came to:
// still loading, an error, or the skills found.
type DetectedState = { status: "loading" } | { status: "error"; error: unknown } | { status: "ok"; items: DetectedSkill[] };

function Form({
  settings,
  library: librarySkills,
  ruleLibrary,
  builtIns,
  builtInsOff,
  detected,
  attention,
  locked,
  canSetCaps,
  canSetLinks,
  canSetTrust,
  canSetQuota,
  canSetStatus,
  instanceTrust,
  platform,
  ciNames,
  linkable,
  onSearch,
  busy,
  onSave,
  onReset,
}: {
  settings: RepoSettings;
  library: LibrarySkill[];
  // ruleLibrary is the rule library; null while it is read (or can't be).
  ruleLibrary: RuleSet[] | null;
  // builtIns are pergent's built-in rules, or where their read is.
  // builtInsOff is why reviews would leave chosen ones out.
  builtIns: BuiltInRule[] | "loading" | "error";
  builtInsOff: string;
  detected: DetectedState;
  attention: string[];
  locked: boolean;
  canSetCaps: boolean;
  canSetLinks: boolean;
  canSetTrust: boolean;
  canSetQuota: boolean;
  canSetStatus: boolean;
  // instanceTrust is the instance's mode, which "Instance default" follows.
  instanceTrust: TrustMode;
  platform: string;
  // ciNames are the job names seen in the last runs; null when not read.
  ciNames: string[] | null;
  linkable: LinkableState;
  onSearch: (text: string) => void;
  busy: boolean;
  onSave: (body: SettingsUpdate) => void;
  onReset: () => void;
}) {
  const library = librarySkills.map((s) => s.name);
  const sizeOf = new Map(librarySkills.map((s) => [s.name, s.size]));
  // The default list, in the library's order.
  const defaultSkills = librarySkills.filter((s) => s.inDefaults).map((s) => s.name);
  // skills null is the default list.
  const skillsNow = settings.skills.value ?? defaultSkills;
  // The default rule sets, by name.
  const defaultRuleSets = (ruleLibrary ?? []).filter((x) => x.inDefaults).map((x) => x.name);
  const initial: Draft = {
    approve: settings.approve.value,
    reviewDrafts: settings.reviewDrafts.value,
    inlineFindings: settings.inlineFindings.value,
    commandsAuthorAndMaintainersOnly: settings.commandsAuthorAndMaintainersOnly.value,
    incrementalReviews: settings.incrementalReviews.value,
    learnFromDismissals: settings.learnFromDismissals.value,
    quietReviews: settings.quietReviews.value,
    checkDeleted: settings.checkDeleted.value,
    ciJobs: tidyCIJobs(settings.ciJobs.value),
    linkedReposEnabled: settings.linkedReposEnabled.value,
    linkedRepos: settings.linkedRepos.value.map((l) => ({ ...l, watched: [...l.watched] })),
    commentLayout: settings.commentLayout.overridden ? settings.commentLayout.value : null,
    failureStatus: settings.failureStatus.value,
    blockingStatus: settings.blockingStatus.value,
    skillsMode: settings.skills.value === null ? "default" : "choose",
    skills: skillsNow,
    repoSkills: settings.repoSkills.value,
    ruleSetsMode: (settings.ruleSets?.value ?? null) === null ? "default" : "choose",
    ruleSets: settings.ruleSets?.value ?? [],
    builtInRules: settings.builtInRules?.value ?? [],
    jobTimeoutSeconds: String(settings.jobTimeoutSeconds.value),
    maxTurns: String(settings.maxTurns.value),
    jobMemory: settings.jobMemory.value,
    rerunLimitPerMR: String(settings.rerunLimitPerMR.value),
    rerunLimitPerRepo: String(settings.rerunLimitPerRepo.value),
    trustedAuthorsOnly:
      settings.trustedAuthorsOnly.overridden && settings.trustedAuthorsOnly.value !== "follow_visibility" ? settings.trustedAuthorsOnly.value : "",
    trustedAuthors: settings.trustedAuthors.value,
    monthlyTokenQuota: quotaText(settings.monthlyTokenQuota?.value ?? null),
  };
  const [newAuthor, setNewAuthor] = useState("");
  const [d, setD] = useState<Draft>(initial);
  const [problem, setProblem] = useState("");
  const set = <K extends keyof Draft>(k: K, v: Draft[K]) => setD((old) => ({ ...old, [k]: v }));
  // Skills the repository names that the library no longer has: shown,
  // but never sent back (the API would refuse them).
  const gone = skillsNow.filter((s) => !library.includes(s));
  const skillNames = [...library, ...gone];
  const disabled = locked || busy;
  // What the review would take of library text, the default list or the choice.
  const used = d.skillsMode === "default" ? defaultSkills : d.skills;
  const total = used.reduce((sum, name) => sum + (sizeOf.get(name) ?? 0), 0);

  // The detected skills that can be turned on, then the names turned on that
  // the folders no longer hold; each name once, in the order the folders list.
  const found = detected.status === "ok" ? detected.items : [];
  const foundNames = [...new Set(found.map((x) => x.name))];
  const missing = initial.repoSkills.filter((n) => !foundNames.includes(n));
  const repoOrder = [...foundNames, ...missing];

  const submit = () => {
    const body: SettingsUpdate = {};
    if (d.approve !== initial.approve) body.approve = d.approve;
    if (d.reviewDrafts !== initial.reviewDrafts) body.reviewDrafts = d.reviewDrafts;
    if (d.inlineFindings !== initial.inlineFindings) body.inlineFindings = d.inlineFindings;
    if (d.commandsAuthorAndMaintainersOnly !== initial.commandsAuthorAndMaintainersOnly) {
      body.commandsAuthorAndMaintainersOnly = d.commandsAuthorAndMaintainersOnly;
    }
    if (d.incrementalReviews !== initial.incrementalReviews) body.incrementalReviews = d.incrementalReviews;
    if (d.learnFromDismissals !== initial.learnFromDismissals) body.learnFromDismissals = d.learnFromDismissals;
    if (d.quietReviews !== initial.quietReviews) body.quietReviews = d.quietReviews;
    if (d.checkDeleted !== initial.checkDeleted) body.checkDeleted = d.checkDeleted;
    // No jobs clears the setting, as null does.
    const ci = tidyCIJobs(d.ciJobs);
    if (JSON.stringify(ci) !== JSON.stringify(initial.ciJobs)) {
      if (ci.jobs.length > 0) body.ciJobs = ci;
      else if (initial.ciJobs.jobs.length > 0) body.ciJobs = null;
    }
    // Who is trusted is an admin's: a maintainer's save never carries it
    // (the server would refuse the whole save).
    if (canSetTrust) {
      if (d.trustedAuthorsOnly !== initial.trustedAuthorsOnly) body.trustedAuthorsOnly = d.trustedAuthorsOnly === "" ? null : d.trustedAuthorsOnly;
      if (JSON.stringify(d.trustedAuthors) !== JSON.stringify(initial.trustedAuthors)) {
        body.trustedAuthors = d.trustedAuthors.length === 0 ? null : d.trustedAuthors;
      }
    }
    // The token quota is an admin's too: it stops the repository's reviews.
    if (canSetQuota && settings.monthlyTokenQuota && d.monthlyTokenQuota.trim() !== initial.monthlyTokenQuota) {
      const quota = parseQuota(d.monthlyTokenQuota);
      if (quota === undefined) {
        setProblem(quotaProblem);
        return;
      }
      if (quota !== settings.monthlyTokenQuota.value) body.monthlyTokenQuota = quota;
    }
    // The linked repositories are an admin's: a maintainer's save never
    // carries them (the server would refuse the whole save). No links is sent
    // as null.
    if (canSetLinks && platform === "github") {
      if (d.linkedReposEnabled !== initial.linkedReposEnabled) body.linkedReposEnabled = d.linkedReposEnabled;
      const links = tidyLinks(d.linkedRepos);
      if (JSON.stringify(links) !== JSON.stringify(tidyLinks(initial.linkedRepos))) {
        body.linkedRepos = links.length === 0 ? null : links;
      }
    }
    // A repository that follows the default and ends up with the list it
    // already shows (a row moved and moved back) has changed nothing: sending
    // it would pin the repository to today's default.
    const followed = initial.commentLayout === null && d.commentLayout !== null && sameLayout(d.commentLayout, settings.commentLayout.value);
    if (!followed && !sameLayout(d.commentLayout, initial.commentLayout)) body.commentLayout = d.commentLayout;
    // The status settings are an admin's: a maintainer's save never carries
    // them (the server would refuse the whole save).
    if (canSetStatus) {
      if (d.failureStatus !== initial.failureStatus) body.failureStatus = d.failureStatus;
      if (d.blockingStatus !== initial.blockingStatus) body.blockingStatus = d.blockingStatus;
    }
    if (d.skillsMode === "default") {
      // Back to the default list: null resets the setting.
      if (initial.skillsMode === "choose") body.skills = null;
    } else if (initial.skillsMode === "default" || !sameSet(d.skills, initial.skills)) {
      // Choosing names the list, even when it holds what the default list does.
      const kept = library.filter((s) => d.skills.includes(s));
      if (kept.length === 0) {
        setProblem("Keep at least one library skill on.");
        return;
      }
      body.skills = kept;
    }
    // None is sent as null, which resets the setting.
    if (!sameSet(d.builtInRules, initial.builtInRules)) body.builtInRules = d.builtInRules.length === 0 ? null : d.builtInRules;
    if (d.ruleSetsMode === "default") {
      // Back to the default list: null resets the setting.
      if (initial.ruleSetsMode === "choose") body.ruleSets = null;
    } else if (initial.ruleSetsMode === "default" || d.ruleSets.join() !== initial.ruleSets.join()) {
      // The order matters: the first entry that matches a file wins.
      const kept = ruleLibrary === null ? d.ruleSets : d.ruleSets.filter((n) => ruleLibrary.some((x) => x.name === n));
      if (kept.length === 0) {
        setProblem("Choose at least one rule set, or use the default rule sets.");
        return;
      }
      body.ruleSets = kept;
    }
    // The detected skills are only sent when they were read: without the
    // read the page doesn't know what the switches would say.
    if (detected.status === "ok" && !sameSet(d.repoSkills, initial.repoSkills)) {
      body.repoSkills = repoOrder.filter((n) => d.repoSkills.includes(n));
    }
    if (d.jobTimeoutSeconds.trim() !== initial.jobTimeoutSeconds) {
      const t = d.jobTimeoutSeconds.trim();
      if (t !== "" && (!/^\d+$/.test(t) || Number(t) < 60 || Number(t) > 7200)) {
        setProblem("The timeout is a whole number of seconds, from 60 to 7200.");
        return;
      }
      body.jobTimeoutSeconds = t === "" ? null : Number(t);
    }
    if (d.maxTurns.trim() !== initial.maxTurns) {
      const t = d.maxTurns.trim();
      if (t !== "" && (!/^\d+$/.test(t) || Number(t) < 5 || Number(t) > 200)) {
        setProblem(stepsProblem);
        return;
      }
      body.maxTurns = t === "" ? null : Number(t);
    }
    if (d.jobMemory.trim() !== initial.jobMemory) {
      body.jobMemory = d.jobMemory.trim() === "" ? null : d.jobMemory.trim();
    }
    // The caps are an admin's: a maintainer's save never carries them (the
    // server would refuse the whole save).
    if (canSetCaps) {
      for (const key of ["rerunLimitPerMR", "rerunLimitPerRepo"] as const) {
        if (d[key].trim() === initial[key]) continue;
        const v = capBody(d[key]);
        if (v === undefined) {
          setProblem(capProblem);
          return;
        }
        body[key] = v;
      }
    }
    if (Object.keys(body).length === 0) {
      setProblem("Nothing changed.");
      return;
    }
    setProblem("");
    onSave(body);
  };

  const managed = locked && <span className="badge cfg">managed by config</span>;
  return (
    <>
      <section className="card" aria-label="Review settings">
        {attention.map((m) => (
          <div className="alert" key={m}>
            <span aria-hidden="true">⚠️</span>
            <span className="prose">{m}</span>
          </div>
        ))}
        <h2>Reviews {managed}</h2>
        <Setting
          name="Approve clean reviews"
          overridden={settings.approve.overridden}
          help="When off, pergent still reviews and comments but never approves."
        >
          <Switch label="Approve clean reviews" checked={d.approve} disabled={disabled} onChange={(v) => set("approve", v)} />
        </Setting>
        <Setting
          name={blockingLabel}
          overridden={settings.blockingStatus.overridden}
          help={blockingHelp}
        >
          <select
            aria-label={blockingLabel}
            value={d.blockingStatus}
            disabled={disabled || !canSetStatus}
            onChange={(e) => set("blockingStatus", e.target.value)}
          >
            <option value="failed">failed (blocks merge)</option>
            <option value="neutral">neutral</option>
          </select>
          {!locked && !canSetStatus && <div className="meta">Only an admin changes this.</div>}
        </Setting>
        <Setting name={failureLabel} overridden={settings.failureStatus.overridden} help={failureHelp}>
          <select
            aria-label={failureLabel}
            value={d.failureStatus}
            disabled={disabled || !canSetStatus}
            onChange={(e) => set("failureStatus", e.target.value)}
          >
            <option value="failed">failed (blocks merge)</option>
            <option value="neutral">neutral</option>
          </select>
          {d.blockingStatus === "neutral" && <div className="meta">{notUsedNote}</div>}
          {!locked && !canSetStatus && <div className="meta">Only an admin changes this.</div>}
        </Setting>
        <Setting
          name="Review drafts"
          overridden={settings.reviewDrafts.overridden}
          help="When off, a draft isn't reviewed until it is marked ready."
        >
          <Switch label="Review drafts" checked={d.reviewDrafts} disabled={disabled} onChange={(v) => set("reviewDrafts", v)} />
        </Setting>
        <Setting
          name="Inline findings"
          overridden={settings.inlineFindings.overridden}
          help="Post each finding as a comment on its lines, with a one-click fix when it is small. Off keeps every finding in the review comment."
        >
          <Switch
            label="Inline findings"
            checked={d.inlineFindings}
            disabled={disabled}
            onChange={(v) => set("inlineFindings", v)}
          />
        </Setting>
        <Setting
          name="Only the author and maintainers use commands"
          overridden={settings.commandsAuthorAndMaintainersOnly.overridden}
          help="When on, only the author of a change and the repository's maintainers and owners can use pergent commands on it. When off, anyone with write access can."
        >
          <Switch
            label="Only the author and maintainers use commands"
            checked={d.commandsAuthorAndMaintainersOnly}
            disabled={disabled}
            onChange={(v) => set("commandsAuthorAndMaintainersOnly", v)}
          />
        </Setting>
        <Setting
          name="Trusted authors only"
          overridden={settings.trustedAuthorsOnly.overridden}
          help="When on, a change from an author without write access and not on the list below isn't reviewed until someone with write access starts it. The instance's default follows the repository's visibility: on for a public repository."
        >
          <select
            aria-label="Trusted authors only"
            value={d.trustedAuthorsOnly}
            disabled={disabled || !canSetTrust}
            onChange={(e) => set("trustedAuthorsOnly", e.target.value as Draft["trustedAuthorsOnly"])}
          >
            <option value="">
              Instance default ({trustModeWords(instanceTrust)})
            </option>
            <option value="on">on</option>
            <option value="off">off</option>
          </select>
        </Setting>
        {canSetQuota && settings.monthlyTokenQuota && (
          <Setting
            name="Monthly token quota"
            overridden={settings.monthlyTokenQuota.overridden}
            help="Tokens per calendar month (UTC), input plus output. Empty: limited only by the instance's quota."
          >
            <input
              type="text"
              inputMode="numeric"
              aria-label="Monthly token quota"
              placeholder="no quota"
              value={d.monthlyTokenQuota}
              disabled={disabled}
              onChange={(e) => set("monthlyTokenQuota", e.target.value)}
            />
          </Setting>
        )}
        <div className="set" role="group" aria-label="Trusted authors">
          <div>
            <div className="name">
              Trusted authors <Badge overridden={settings.trustedAuthors.overridden} />
            </div>
            <div className="help">
              People without write access whose changes are reviewed without anyone starting them, such as a regular outside
              contributor or a bot. A username is looked up when you save, and kept by the platform's user ID.
            </div>
            {d.trustedAuthors.length === 0 && <div className="meta">Nobody.</div>}
            {d.trustedAuthors.map((a) => (
              <div className="row" key={`${a.id ?? "new"}/${a.username}`}>
                <span className="grow mono">{a.username}</span>
                {!disabled && canSetTrust && (
                  <button
                    type="button"
                    className="btn small"
                    aria-label={`Remove ${a.username}`}
                    onClick={() =>
                      set(
                        "trustedAuthors",
                        d.trustedAuthors.filter((x) => x !== a),
                      )
                    }
                  >
                    Remove
                  </button>
                )}
              </div>
            ))}
            {!locked && !canSetTrust && <div className="meta">Only admins of this repository change who is trusted.</div>}
          </div>
          {!disabled && canSetTrust && (
            <div className="search">
              <input
                aria-label="Add a username"
                placeholder="username"
                maxLength={255}
                value={newAuthor}
                onChange={(e) => setNewAuthor(e.target.value)}
              />
              <button
                type="button"
                className="btn small"
                onClick={() => {
                  const name = newAuthor.trim();
                  if (name !== "" && !d.trustedAuthors.some((a) => a.username.toLowerCase() === name.toLowerCase())) {
                    set("trustedAuthors", [...d.trustedAuthors, { username: name }]);
                  }
                  setNewAuthor("");
                }}
              >
                Add
              </button>
            </div>
          )}
        </div>
        <Setting
          name="Incremental reviews"
          overridden={settings.incrementalReviews.overridden}
          help="After a push, review only the files changed since the last review."
        >
          <Switch
            label="Incremental reviews"
            checked={d.incrementalReviews}
            disabled={disabled}
            onChange={(v) => set("incrementalReviews", v)}
          />
        </Setting>
        <Setting
          name="Learn from dismissals"
          overridden={settings.learnFromDismissals.overridden}
          help="Later reviews are told what people with write access rejected, and why."
        >
          <Switch
            label="Learn from dismissals"
            checked={d.learnFromDismissals}
            disabled={disabled}
            onChange={(v) => set("learnFromDismissals", v)}
          />
        </Setting>
        <Setting
          name="Quiet reviews"
          overridden={settings.quietReviews.overridden}
          help="Fewer nitpicks, no suggestion threads and a shorter comment. Blocking findings, failures and approval stay the same."
        >
          <Switch
            label="Quiet reviews"
            checked={d.quietReviews}
            disabled={disabled}
            onChange={(v) => set("quietReviews", v)}
          />
        </Setting>
        <Setting
          name="Review deleted and renamed files"
          overridden={settings.checkDeleted.overridden}
          help="Reviews the lines a change deletes, and checks what still uses deleted files and renamed files' old paths. Costs more tokens. Needs pergent v0.10.0 or newer; an older image ignores it."
        >
          <Switch
            label="Review deleted and renamed files"
            checked={d.checkDeleted}
            disabled={disabled}
            onChange={(v) => set("checkDeleted", v)}
          />
        </Setting>
      </section>

      <section className="card" aria-label="CI jobs">
        <h2>
          CI jobs <Badge overridden={settings.ciJobs.overridden} /> {managed}
        </h2>
        <div className="pad meta">
          Choose the CI jobs a review waits for. A blocking job that has not passed holds approval back. A job read as log or report
          also gives the review its output.
        </div>
        <CIJobsEditor value={d.ciJobs} onChange={(v) => set("ciJobs", v)} names={ciNames} github={platform === "github"} disabled={disabled} />
      </section>

      {canSetLinks && (
        <section className="card" aria-label="Linked repositories">
          <h2>
            Linked repositories <Badge overridden={settings.linkedReposEnabled.overridden || settings.linkedRepos.overridden} /> {managed}
          </h2>
          {platform === "github" ? (
            <LinkedReposEditor
              enabled={d.linkedReposEnabled}
              onEnabled={(v) => set("linkedReposEnabled", v)}
              links={d.linkedRepos}
              onLinks={(v) => set("linkedRepos", v)}
              linkable={linkable}
              onSearch={onSearch}
              canAdd={!locked}
              disabled={disabled}
            />
          ) : (
            <div className="pad meta">Linked repositories work on GitHub repositories only, for now.</div>
          )}
        </section>
      )}

      <section className="card" aria-label="Comment layout">
        <h2>
          Comment layout <Badge overridden={settings.commentLayout.overridden} /> {managed}
        </h2>
        <div className="pad meta">
          The blocks of the review comment, in order. A block that is not shown is left out. Quiet reviews and the size limit
          can still cut what a block shows.
        </div>
        <CommentLayoutEditor
          value={d.commentLayout}
          // While the repository sets none, its value is the default in effect.
          defaults={settings.commentLayout.overridden ? undefined : settings.commentLayout.value}
          disabled={disabled}
          onChange={(v) => set("commentLayout", v)}
        />
      </section>

      <section className="card" aria-label="Skills used for this repository">
        <h2>
          Skills used for this repository <Badge overridden={settings.skills.overridden} /> {managed}
        </h2>
        <div className="skill">
          <label className="grow">
            <input
              type="radio"
              name="skills-mode"
              checked={d.skillsMode === "default"}
              disabled={disabled}
              onChange={() => set("skillsMode", "default")}
            />{" "}
            Use the default skills ({defaultSkills.length > 0 ? defaultSkills.join(", ") : "none"})
          </label>
        </div>
        <div className="skill">
          <label className="grow">
            <input
              type="radio"
              name="skills-mode"
              checked={d.skillsMode === "choose"}
              disabled={disabled}
              onChange={() => set("skillsMode", "choose")}
            />{" "}
            Choose skills
          </label>
        </div>
        {d.skillsMode === "default" && defaultSkills.length === 0 && (
          <div className="pad meta">
            There are no default skills, so this repository gets no skills from the library. Without skills of its own it is
            not reviewed.
          </div>
        )}
        {(d.skillsMode === "choose" || locked) &&
          skillNames.map((s) => (
            <div className="skill" key={s}>
              <Switch
                label={`Skill ${s}`}
                checked={used.includes(s)}
                disabled={disabled || d.skillsMode === "default"}
                onChange={(on) => set("skills", on ? [...d.skills, s] : d.skills.filter((x) => x !== s))}
              />
              <b className="grow">{s}</b>
              <span className="src">{library.includes(s) ? "library" : "not in the library"}</span>
            </div>
          ))}
        <div className={total > skillBudget ? "pad problem" : "pad meta"}>
          Total {bytesText(total)} of {bytesText(skillBudget)}
          {total > skillBudget && ": a review takes at most that much library skill text."}
        </div>
        {gone.length > 0 && (
          <div className="pad meta">
            {gone.join(", ")} {gone.length === 1 ? "is" : "are"} no longer in the library. Saving a change to the skills
            removes {gone.length === 1 ? "it" : "them"} from this repository.
          </div>
        )}
      </section>

      <section className="card" aria-label="Rule sets used for this repository">
        <h2>
          Rule sets used for this repository <Badge overridden={settings.ruleSets?.overridden ?? false} /> {managed}
        </h2>
        <div className="skill">
          <label className="grow">
            <input
              type="radio"
              name="rule-sets-mode"
              checked={d.ruleSetsMode === "default"}
              disabled={disabled}
              onChange={() => set("ruleSetsMode", "default")}
            />{" "}
            Use the default rule sets ({defaultRuleSets.length > 0 ? defaultRuleSets.join(", ") : "none"})
          </label>
        </div>
        <div className="skill">
          <label className="grow">
            <input
              type="radio"
              name="rule-sets-mode"
              checked={d.ruleSetsMode === "choose"}
              disabled={disabled}
              onChange={() => set("ruleSetsMode", "choose")}
            />{" "}
            Choose rule sets
          </label>
        </div>
        {d.ruleSetsMode === "choose" &&
          (ruleLibrary === null ? (
            <Loading />
          ) : (
            <RuleSetPicker
              label="Rule sets"
              library={ruleLibrary}
              selected={d.ruleSets}
              disabled={disabled}
              onChange={(v) => set("ruleSets", v)}
            />
          ))}
        <div className="pad meta">
          After the repository's own .pergent/rules.yaml, in this order. The first entry that matches a file gives it its rule;
          a rule never says what is blocking.
        </div>
      </section>

      <section className="card" aria-label="Built-in rules">
        <h2>
          Built-in rules <Badge overridden={settings.builtInRules?.overridden ?? false} /> {managed}
        </h2>
        <div className="pad meta">
          pergent's own path rules, after the repository's and the rule sets'. Each one adds its text to the runs on its files, so
          enable only the ones you need. None is enabled by default.
        </div>
        {builtInsOff !== "" && <div className={d.builtInRules.length > 0 ? "pad problem" : "pad meta"}>{builtInsOff}</div>}
        {builtIns === "loading" && <Loading />}
        {builtIns === "error" && (
          <div className="pad meta">
            The built-in rules can't be read now{d.builtInRules.length > 0 && `. Enabled: ${d.builtInRules.join(", ")}`}.
          </div>
        )}
        {Array.isArray(builtIns) && (
          <BuiltInRulePicker rules={builtIns} selected={d.builtInRules} disabled={disabled} onChange={(v) => set("builtInRules", v)} />
        )}
      </section>

      <section className="card" aria-label="Skills in the repository">
        <h2>Skills in the repository {managed}</h2>
        <div className="skill">
          <span className="grow">
            <b>.pergent/skills</b>
          </span>
          <span className="src">always on</span>
        </div>
        {detected.status === "loading" && <Loading />}
        {detected.status === "error" && <ErrorBox error={detected.error} />}
        {detected.status === "ok" &&
          found.map((x) => (
            <div className="skill" key={`${x.folder}/${x.name}`}>
              <Switch
                label={`Repository skill ${x.name}`}
                checked={d.repoSkills.includes(x.name)}
                disabled={disabled || (x.problem !== undefined && x.problem !== "" && !d.repoSkills.includes(x.name))}
                onChange={(on) => set("repoSkills", on ? [...d.repoSkills, x.name] : d.repoSkills.filter((n) => n !== x.name))}
              />
              <b className="grow">{x.name}</b>
              {x.problem && <span className="problem">{x.problem}</span>}
              <span className="src">{x.folder}</span>
            </div>
          ))}
        {detected.status === "ok" &&
          missing.map((n) => (
            <div className="skill" key={`missing/${n}`}>
              <Switch
                label={`Repository skill ${n}`}
                checked={d.repoSkills.includes(n)}
                disabled={disabled}
                onChange={(on) => set("repoSkills", on ? [...d.repoSkills, n] : d.repoSkills.filter((x) => x !== n))}
              />
              <b className="grow">{n}</b>
              <span className="problem">not found in the repository</span>
            </div>
          ))}
        {detected.status === "ok" && found.length === 0 && missing.length === 0 && (
          <div className="pad meta">No skills in .agents/skills, .claude/skills or .github/skills.</div>
        )}
        <div className="pad meta">
          Skills in .agents/skills, .claude/skills and .github/skills run only when turned on here; .pergent/skills always runs.
          A skill in a repository folder replaces a library skill of the same name. Only the SKILL.md text of a skill is used:
          its other files are ignored.
        </div>
      </section>

      <section className="card" aria-label="Review Job">
        <h2>Review Job {managed}</h2>
        <Setting
          name="Timeout"
          overridden={settings.jobTimeoutSeconds.overridden}
          help="How long one review may run, in seconds (60 to 7200)."
        >
          <input
            type="text"
            inputMode="numeric"
            aria-label="Timeout (seconds)"
            value={d.jobTimeoutSeconds}
            disabled={disabled}
            onChange={(e) => set("jobTimeoutSeconds", e.target.value)}
          />
        </Setting>
        <Setting
          name="Max steps"
          overridden={settings.maxTurns.overridden}
          help="Steps each opencode run may take before pergent stops it; each step can call several tools."
        >
          <input
            type="text"
            inputMode="numeric"
            aria-label="Max steps"
            value={d.maxTurns}
            disabled={disabled}
            onChange={(e) => set("maxTurns", e.target.value)}
          />
        </Setting>
        <Setting
          name="Memory limit"
          overridden={settings.jobMemory.overridden}
          help="Raise it if reviews run out of memory, for example 6Gi. Empty means the default."
        >
          <input
            type="text"
            aria-label="Memory limit"
            placeholder="none"
            value={d.jobMemory}
            disabled={disabled}
            onChange={(e) => set("jobMemory", e.target.value)}
          />
        </Setting>
        <Setting
          name="Re-run cap per MR/PR"
          overridden={settings.rerunLimitPerMR.overridden}
          help="The most re-runs (buttons and comments) of one merge request or pull request in a UTC day, 1 to 1000. Pushes always run."
        >
          <input
            type="number"
            aria-label="Re-run cap per MR/PR (per day)"
            min={1}
            max={1000}
            step={1}
            value={d.rerunLimitPerMR}
            disabled={locked || busy}
            readOnly={!canSetCaps}
            onChange={(e) => set("rerunLimitPerMR", e.target.value)}
          />
        </Setting>
        <Setting
          name="Re-run cap per repository"
          overridden={settings.rerunLimitPerRepo.overridden}
          help="The most re-runs across the whole repository in a UTC day, 1 to 1000."
        >
          <input
            type="number"
            aria-label="Re-run cap per repository (per day)"
            min={1}
            max={1000}
            step={1}
            value={d.rerunLimitPerRepo}
            disabled={locked || busy}
            readOnly={!canSetCaps}
            onChange={(e) => set("rerunLimitPerRepo", e.target.value)}
          />
        </Setting>
        {!locked && !canSetCaps && (
          <div className="pad meta">Only admins of this repository change its re-run caps.</div>
        )}
        {!locked && (
          <div className="foot">
            {problem && <span className="meta problem">{problem}</span>}
            <button type="button" className="btn" disabled={busy} onClick={onReset}>
              Reset to defaults
            </button>
            <button type="button" className="btn primary" disabled={busy} onClick={submit}>
              Save
            </button>
          </div>
        )}
        {locked && <div className="foot meta">This repository is listed in pergent.sh's config; change it there.</div>}
      </section>
    </>
  );
}

function Badge({ overridden }: { overridden: boolean }) {
  return overridden ? <span className="badge">overridden here</span> : <span className="badge cfg">default</span>;
}

function Setting({ name, overridden, help, children }: { name: string; overridden: boolean; help: string; children: ReactNode }) {
  return (
    <div className="set">
      <div>
        <div className="name">
          {name} <Badge overridden={overridden} />
        </div>
        <div className="help">{help}</div>
      </div>
      <div>{children}</div>
    </div>
  );
}
