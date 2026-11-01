import { useMutation, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";
import { Link } from "react-router";

import { paths, type FetchedSkill, type SkillFetchResult } from "../api";
import { useMe } from "../layout/me";
import { useWrite } from "../layout/write";
import { FetchStatus, useSkillFetch, useStartFetch } from "../skillFetch";
import { ErrorBox, Loading, Pill, bytesText } from "../ui";

// SkillInstall installs skills into the library from a git source (admins):
// a repository of a connected instance, or a public URL. It reads the source
// first, lists the skills found, and installs the ticked ones.
export default function SkillInstall() {
  const me = useMe();
  const [kind, setKind] = useState<"instance" | "url">("instance");
  const [instance, setInstance] = useState(me.instance);
  const [repo, setRepo] = useState("");
  const [url, setUrl] = useState("");
  const [ref, setRef] = useState("");
  const [folder, setFolder] = useState("");
  const [id, setId] = useState<string>();
  const start = useStartFetch(setId);
  const read = useSkillFetch(id);
  const running = start.isPending || read.data?.status === "running";
  const submit = (e: FormEvent) => {
    e.preventDefault();
    const source = kind === "url" ? { url: url.trim() } : { instance: instance.trim(), repo: repo.trim() };
    start.mutate({
      ...source,
      ref: ref.trim(),
      path: folder.trim(),
      purpose: "install",
    });
  };
  return (
    <div className="narrow">
      <div className="crumbs">
        <Link to="/skills">Skills</Link> / Install from git
      </div>
      <div className="top">
        <h1>Install from git</h1>
      </div>
      <p className="meta">
        Installed skills are readable by every signed-in user, including skills from a private repository. Install only what
        everyone may read.
      </p>
      <form className="card pad stack" aria-label="Read a source" onSubmit={submit}>
        <div className="actions-row">
          <label className="check plain">
            <input type="radio" name="source" checked={kind === "instance"} onChange={() => setKind("instance")} />A repository on
            an instance
          </label>
          <label className="check plain">
            <input type="radio" name="source" checked={kind === "url"} onChange={() => setKind("url")} />A public URL
          </label>
        </div>
        {kind === "instance" ? (
          <>
            <div>
              <label htmlFor="fetch-instance">Instance</label>
              <input
                id="fetch-instance"
                type="text"
                autoComplete="off"
                value={instance}
                disabled={!me.operator}
                onChange={(e) => setInstance(e.target.value)}
                required
              />
              {!me.operator && (
                <div className="meta">You can install from repositories of your own instance that you can see.</div>
              )}
            </div>
            <div>
              <label htmlFor="fetch-repo">Repository</label>
              <input
                id="fetch-repo"
                type="text"
                autoComplete="off"
                placeholder="group/project"
                value={repo}
                onChange={(e) => setRepo(e.target.value)}
                required
              />
            </div>
          </>
        ) : (
          <div>
            <label htmlFor="fetch-url">URL</label>
            <input
              id="fetch-url"
              type="text"
              autoComplete="off"
              placeholder="https://example.com/skills.git"
              value={url}
              onChange={(e) => setUrl(e.target.value)}
              required
            />
            <div className="meta">
              A public https git URL, read in a Job that holds no token. The Job reaches only jobEgress.cidrs, so the host must be
              in that list.
            </div>
          </div>
        )}
        <div>
          <label htmlFor="fetch-ref">Branch or tag</label>
          <input
            id="fetch-ref"
            type="text"
            autoComplete="off"
            placeholder="main or v1.0"
            value={ref}
            onChange={(e) => setRef(e.target.value)}
            required
          />
        </div>
        <div>
          <label htmlFor="fetch-folder">Folder</label>
          <input
            id="fetch-folder"
            type="text"
            autoComplete="off"
            placeholder="skills"
            value={folder}
            onChange={(e) => setFolder(e.target.value)}
          />
          <div className="meta">Leave it empty to read the top of the repository. Each skill is name.md or name/SKILL.md.</div>
        </div>
        <div className="actions-row">
          <button type="submit" className="btn primary" disabled={running}>
            Read skills
          </button>
        </div>
        {start.isError && <ErrorBox error={start.error} />}
      </form>
      {read.isPending && id !== undefined && <Loading />}
      {read.isError && <ErrorBox error={read.error} />}
      {read.isSuccess && read.data.status !== "done" && (
        <section className="card" aria-label="Reading">
          <FetchStatus read={read.data} />
        </section>
      )}
      {read.isSuccess && read.data.status === "done" && (
        <Found key={read.data.id} id={read.data.id} commit={read.data.commit} skills={read.data.skills} />
      )}
    </div>
  );
}

function Found({ id, commit, skills }: { id: string; commit: string; skills: FetchedSkill[] }) {
  const client = useQueryClient();
  const write = useWrite();
  const [ticked, setTicked] = useState<string[]>([]);
  const [addToDefaults, setAddToDefaults] = useState(false);
  const [results, setResults] = useState<Record<string, SkillFetchResult>>({});
  const install = useMutation({
    mutationFn: (names: string[]) =>
      write<SkillFetchResult[]>("POST", paths.apiSkillFetchAction(id, "install"), { names, addToDefaults }),
    onSuccess: async (rs) => {
      setResults((old) => ({
        ...old,
        ...Object.fromEntries(rs.map((r) => [r.name, r])),
      }));
      setTicked((old) => old.filter((n) => !rs.some((r) => r.name === n && r.ok)));
      if (rs.some((r) => r.ok)) {
        void client.invalidateQueries({ queryKey: ["skills"] });
        // The default list may have changed: repositories show it.
        if (addToDefaults) void client.invalidateQueries({ queryKey: ["repositories"] });
        await client.invalidateQueries({ queryKey: ["skill-fetch", id] });
      }
    },
  });
  const toggle = (name: string, on: boolean) => setTicked((old) => (on ? [...old, name] : old.filter((n) => n !== name)));
  const submit = (e: FormEvent) => {
    e.preventDefault();
    install.mutate(ticked);
  };
  return (
    <section className="card" aria-label="Skills found">
      <form onSubmit={submit}>
        <div className="pad meta">{commit ? `Read commit ${commit.slice(0, 7)}.` : "Read."}</div>
        {skills.length === 0 && <div className="row empty-row">No skills found in this folder.</div>}
        {skills.map((s) => {
          const result = results[s.name];
          const installed = result?.ok === true;
          const blocked = s.taken || !!s.problem || installed;
          return (
            <div className="check" key={s.path}>
              <input
                type="checkbox"
                aria-label={s.name}
                disabled={blocked}
                checked={ticked.includes(s.name)}
                onChange={(e) => toggle(s.name, e.target.checked)}
              />
              <div className="grow">
                <b>{s.name}</b> <span className="meta">{s.path}</span>
                {s.size > 0 && <span className="meta"> · {bytesText(s.size)}</span>}
                {s.problem && <div className="problem">{s.problem}</div>}
                {s.taken && !installed && <div className="meta">name already in the library</div>}
                {installed && <Pill tone="ok">Installed</Pill>}
                {result && !result.ok && <div className="problem">{result.error}</div>}
              </div>
            </div>
          );
        })}
        <div className="pad stack">
          <label className="check plain">
            <input type="checkbox" checked={addToDefaults} onChange={(e) => setAddToDefaults(e.target.checked)} />
            Add to default skills
          </label>
          <div className="meta">A repository that sets no skills of its own uses the default skills.</div>
          <div className="actions-row">
            <button type="submit" className="btn primary" disabled={install.isPending || ticked.length === 0}>
              Install
            </button>
          </div>
          {install.isError && <ErrorBox error={install.error} />}
        </div>
      </form>
    </section>
  );
}
