import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState, type ReactNode } from "react";

import {
  api,
  paths,
  type LibrarySkill,
  type RepoSettings,
  type RepositoryDetailView,
  type SettingsUpdate,
} from "../api";
import { useWrite } from "../layout/write";
import { ErrorBox, Loading, Switch } from "../ui";

// SettingsTab edits a repository's settings (admins only; Layout hides the
// tab from others). A config-managed repository's are shown disabled.
// queryKey is the page's own: the address may spell the project
// differently from the server.
export default function SettingsTab({ view, queryKey }: { view: RepositoryDetailView; queryKey: unknown[] }) {
  const client = useQueryClient();
  const write = useWrite();
  const { repo } = view;
  const library = useQuery({ queryKey: ["skills"], queryFn: () => api.get<LibrarySkill[]>("/api/skills") });
  const [note, setNote] = useState("");

  const done = (detail: RepositoryDetailView, message: string) => {
    client.setQueryData(queryKey, detail);
    client.invalidateQueries({ queryKey: ["repositories"] });
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
        library={library.data.map((s) => s.name)}
        attention={view.attention.map((a) => a.message)}
        locked={locked}
        busy={busy}
        onSave={(body) => save.mutate(body)}
        onReset={() => {
          if (window.confirm("Put every setting of this repository back to its default?")) reset.mutate();
        }}
      />
      {note && (
        <div className="note" role="status">
          {note}
        </div>
      )}
      {save.isError && <ErrorBox error={save.error} />}
      {reset.isError && <ErrorBox error={reset.error} />}
    </>
  );
}

type Draft = {
  approve: boolean;
  reviewDrafts: boolean;
  failureStatus: string;
  skills: string[];
  jobTimeoutSeconds: string;
  jobMemory: string;
};

function sameSet(a: string[], b: string[]) {
  return a.length === b.length && a.every((x) => b.includes(x));
}

function Form({
  settings,
  library,
  attention,
  locked,
  busy,
  onSave,
  onReset,
}: {
  settings: RepoSettings;
  library: string[];
  attention: string[];
  locked: boolean;
  busy: boolean;
  onSave: (body: SettingsUpdate) => void;
  onReset: () => void;
}) {
  // skills null means every library skill.
  const skillsNow = settings.skills.value ?? library;
  const initial: Draft = {
    approve: settings.approve.value,
    reviewDrafts: settings.reviewDrafts.value,
    failureStatus: settings.failureStatus.value,
    skills: skillsNow,
    jobTimeoutSeconds: String(settings.jobTimeoutSeconds.value),
    jobMemory: settings.jobMemory.value,
  };
  const [d, setD] = useState<Draft>(initial);
  const [problem, setProblem] = useState("");
  const set = <K extends keyof Draft>(k: K, v: Draft[K]) => setD((old) => ({ ...old, [k]: v }));
  // Skills the repository names that the library no longer has: shown,
  // but never sent back (the API would refuse them).
  const gone = skillsNow.filter((s) => !library.includes(s));
  const skillNames = [...library, ...gone];
  const disabled = locked || busy;

  const submit = () => {
    const body: SettingsUpdate = {};
    if (d.approve !== initial.approve) body.approve = d.approve;
    if (d.reviewDrafts !== initial.reviewDrafts) body.reviewDrafts = d.reviewDrafts;
    if (d.failureStatus !== initial.failureStatus) body.failureStatus = d.failureStatus;
    if (!sameSet(d.skills, initial.skills)) {
      const kept = library.filter((s) => d.skills.includes(s));
      if (kept.length === 0) {
        setProblem("Keep at least one library skill on.");
        return;
      }
      body.skills = kept;
    }
    if (d.jobTimeoutSeconds.trim() !== initial.jobTimeoutSeconds) {
      const t = d.jobTimeoutSeconds.trim();
      if (t !== "" && !/^\d+$/.test(t)) {
        setProblem("The timeout is a whole number of seconds, from 60 to 7200.");
        return;
      }
      body.jobTimeoutSeconds = t === "" ? null : Number(t);
    }
    if (d.jobMemory.trim() !== initial.jobMemory) {
      body.jobMemory = d.jobMemory.trim() === "" ? null : d.jobMemory.trim();
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
          name="Status when a review fails"
          overridden={settings.failureStatus.overridden}
          help={`"neutral" won't block merging when the LLM provider is down.`}
        >
          <select
            aria-label="Status when a review fails"
            value={d.failureStatus}
            disabled={disabled}
            onChange={(e) => set("failureStatus", e.target.value)}
          >
            <option value="failed">failed (blocks merge)</option>
            <option value="neutral">neutral</option>
          </select>
        </Setting>
        <Setting
          name="Review drafts"
          overridden={settings.reviewDrafts.overridden}
          help="When off, a draft isn't reviewed until it is marked ready."
        >
          <Switch label="Review drafts" checked={d.reviewDrafts} disabled={disabled} onChange={(v) => set("reviewDrafts", v)} />
        </Setting>
      </section>

      <section className="card" aria-label="Skills used for this repository">
        <h2>
          Skills used for this repository <Badge overridden={settings.skills.overridden} /> {managed}
        </h2>
        {skillNames.map((s) => (
          <div className="skill" key={s}>
            <Switch
              label={`Skill ${s}`}
              checked={d.skills.includes(s)}
              disabled={disabled}
              onChange={(on) => set("skills", on ? [...d.skills, s] : d.skills.filter((x) => x !== s))}
            />
            <b className="grow">{s}</b>
            <span className="src">{library.includes(s) ? "library" : "not in the library"}</span>
          </div>
        ))}
        {gone.length > 0 && (
          <div className="pad meta">
            {gone.join(", ")} {gone.length === 1 ? "is" : "are"} no longer in the library. Saving a change to the skills
            removes {gone.length === 1 ? "it" : "them"} from this repository.
          </div>
        )}
      </section>

      <section className="card" aria-label="Review Job">
        <h2>Review Job {managed}</h2>
        <Setting
          name="Timeout"
          overridden={settings.jobTimeoutSeconds.overridden}
          help="How long one review may run, in seconds (60 to 7200)."
        >
          <input
            type="number"
            aria-label="Timeout (seconds)"
            min={60}
            max={7200}
            step={1}
            value={d.jobTimeoutSeconds}
            disabled={disabled}
            onChange={(e) => set("jobTimeoutSeconds", e.target.value)}
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
