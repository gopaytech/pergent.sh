import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";
import { Link, useNavigate, useParams } from "react-router";

import { api, isStatus, paths, type SkillDetail, type SkillVersion, type SkillVersionDetail } from "../api";
import { useMe } from "../layout/me";
import { useWrite } from "../layout/write";
import { ErrorBox, Loading, NotFound, Pill, SkillSourceText, ago, bytesText } from "../ui";

// maxSkillBytes is the most a skill may be (inputs.MaxSkillBytes, 128 KiB):
// the counter warns and Save waits; the server checks again.
const maxSkillBytes = 128 * 1024;

const sizeOf = (text: string) => new TextEncoder().encode(text).length;

function SizeCounter({ text }: { text: string }) {
  const n = sizeOf(text);
  return (
    <span className={n > maxSkillBytes ? "problem" : "meta"}>
      {bytesText(n)} of {bytesText(maxSkillBytes)}
    </span>
  );
}

// NewSkill is the form that writes a skill into the library (admins).
// Add to default skills is off at first, so the skill reaches no repository
// until someone chooses it.
export function NewSkill() {
  const client = useQueryClient();
  const write = useWrite();
  const navigate = useNavigate();
  const [name, setName] = useState("");
  const [content, setContent] = useState("");
  const [addToDefaults, setAddToDefaults] = useState(false);
  const create = useMutation({
    mutationFn: () => write("POST", "/api/admin/skills", { name: name.trim(), content, addToDefaults }),
    onSuccess: async () => {
      await client.invalidateQueries({ queryKey: ["skills"] });
      // The default list may have changed: repositories show it.
      void client.invalidateQueries({ queryKey: ["repositories"] });
      navigate("/skills");
    },
  });
  const submit = (e: FormEvent) => {
    e.preventDefault();
    create.mutate();
  };
  return (
    <div className="narrow">
      <div className="crumbs">
        <Link to="/skills">Skills</Link> / New skill
      </div>
      <div className="top">
        <h1>New skill</h1>
      </div>
      <form className="card pad stack" aria-label="New skill" onSubmit={submit}>
        <div>
          <label htmlFor="skill-name">Name</label>
          <input
            id="skill-name"
            type="text"
            autoComplete="off"
            maxLength={100}
            pattern="[A-Za-z0-9][A-Za-z0-9._\-]*"
            title="Letters, digits, dots, dashes and underscores, starting with a letter or digit"
            value={name}
            onChange={(e) => setName(e.target.value)}
            required
          />
          <div className="meta">The name can't change later.</div>
        </div>
        <div>
          <label htmlFor="skill-text">Skill text</label>
          <textarea
            id="skill-text"
            className="skill-text"
            spellCheck={false}
            value={content}
            onChange={(e) => setContent(e.target.value)}
            required
          />
          <div className="meta">
            A markdown file. An optional frontmatter may set paths, exclude and cross_batch; its name must match the name above.
          </div>
        </div>
        <label className="check">
          <input type="checkbox" checked={addToDefaults} onChange={(e) => setAddToDefaults(e.target.checked)} />
          Add to default skills
        </label>
        <div className="meta">A repository that sets no skills of its own uses the default skills.</div>
        <div className="actions-row">
          <button type="submit" className="btn primary" disabled={create.isPending || sizeOf(content) > maxSkillBytes}>
            Create skill
          </button>
          <SizeCounter text={content} />
        </div>
        {create.isError && <ErrorBox error={create.error} />}
      </form>
    </div>
  );
}

// SkillEditor is one skill: its text and history for everyone, and for an
// admin the editor, when the skill was written in the dashboard. An
// installed skill is read-only: it is copied from the list instead.
export default function SkillEditor() {
  const { name = "" } = useParams();
  const skill = useQuery({ queryKey: ["skill", name], queryFn: () => api.get<SkillDetail>(paths.apiSkill(name)) });
  if (skill.isPending) return <Loading />;
  if (skill.isError) return isStatus(skill.error, 404) ? <NotFound /> : <ErrorBox error={skill.error} />;
  // The key gives each skill its own draft and opened version: moving to
  // another cached skill must not keep the first one's.
  return <SkillPage key={skill.data.name} skill={skill.data} reload={() => skill.refetch()} />;
}

function SkillPage({ skill, reload }: { skill: SkillDetail; reload: () => Promise<{ data?: SkillDetail }> }) {
  const me = useMe();
  const client = useQueryClient();
  const write = useWrite();
  // base is the version and text the draft was opened from. A refetch
  // (window focus, another admin's save) changes skill, never base: Save
  // names the version the draft is based on, so a change made since then is
  // refused as stale (D9), not overwritten.
  const [base, setBase] = useState({ version: skill.version, content: skill.content });
  const [draft, setDraft] = useState(skill.content);
  const [note, setNote] = useState("");
  const editable = me.admin && skill.source === null;
  const save = useMutation({
    mutationFn: (body: { content: string; version: number }) =>
      write<{ version: number }>("PUT", paths.apiAdminSkill(skill.name), body),
    onMutate: () => setNote(""),
    onSuccess: async (r, body) => {
      setBase({ version: r.version, content: body.content });
      setNote(`Saved as v${r.version}.`);
      // Wait for the new version: a second save names it.
      await client.invalidateQueries({ queryKey: ["skill", skill.name] });
      void client.invalidateQueries({ queryKey: ["skills"] });
    },
  });
  const submit = (e: FormEvent) => {
    e.preventDefault();
    save.mutate({ content: draft, version: base.version });
  };
  // Reload puts the stored text, and its version, in place of the draft.
  const reloadText = async () => {
    save.reset();
    setNote("");
    const fresh = await reload();
    if (fresh.data) {
      setBase({ version: fresh.data.version, content: fresh.data.content });
      setDraft(fresh.data.content);
    }
  };
  const stale = save.isError && isStatus(save.error, 409);
  return (
    <div className="narrow">
      <div className="crumbs">
        <Link to="/skills">Skills</Link> / {skill.name}
      </div>
      <div className="top">
        <div>
          <h1>
            {skill.name} {skill.inDefaults && <Pill tone="run">default</Pill>}
          </h1>
          <div className="sub">
            v{skill.version} · <SkillSourceText source={skill.source} />
            {skill.source?.commit ? ` · commit ${skill.source.commit.slice(0, 7)}` : ""} · {bytesText(skill.size)}
          </div>
        </div>
      </div>
      {editable ? (
        <form className="card pad stack" aria-label="Edit skill" onSubmit={submit}>
          <label htmlFor="skill-text">Skill text</label>
          <textarea
            id="skill-text"
            className="skill-text"
            spellCheck={false}
            value={draft}
            onChange={(e) => setDraft(e.target.value)}
          />
          <div className="actions-row">
            <button
              type="submit"
              className="btn primary"
              disabled={save.isPending || draft === base.content || draft.trim() === "" || sizeOf(draft) > maxSkillBytes}
            >
              Save
            </button>
            <SizeCounter text={draft} />
          </div>
          {note && (
            <div className="note" role="status">
              {note}
            </div>
          )}
          {save.isError && <ErrorBox error={save.error} />}
          {stale && (
            <div className="actions-row">
              <button type="button" className="btn small" onClick={() => void reloadText()}>
                Reload
              </button>
              <span className="meta">Reload shows the saved text and drops your changes.</span>
            </div>
          )}
        </form>
      ) : (
        <section className="card" aria-label="Skill text">
          {me.admin && <div className="pad meta">An installed skill can't be edited. Copy it from the list to change it.</div>}
          <pre className="doc">{skill.content}</pre>
        </section>
      )}
      <Versions name={skill.name} versions={skill.versions} />
    </div>
  );
}

function Versions({ name, versions }: { name: string; versions: SkillVersion[] }) {
  const [shown, setShown] = useState<number | null>(null);
  const one = useQuery({
    queryKey: ["skill", name, "version", shown],
    queryFn: () => api.get<SkillVersionDetail>(paths.apiSkillVersion(name, shown ?? 0)),
    enabled: shown !== null,
  });
  return (
    <section className="card" aria-label="Versions">
      <h2>Versions</h2>
      {versions.map((v) => (
        <div className="row" key={v.version}>
          <b>v{v.version}</b>
          <span className="grow meta">
            {ago(v.at)}
            {v.actor ? ` · ${v.actor}` : ""}
            {v.commit ? ` · commit ${v.commit.slice(0, 7)}` : ""}
          </span>
          <button
            type="button"
            className="btn small"
            aria-label={`View v${v.version}`}
            onClick={() => setShown(shown === v.version ? null : v.version)}
          >
            {shown === v.version ? "Hide" : "View"}
          </button>
        </div>
      ))}
      {shown !== null && one.isPending && <Loading />}
      {shown !== null && one.isError && <ErrorBox error={one.error} />}
      {shown !== null && one.isSuccess && <pre className="doc">{one.data.content}</pre>}
    </section>
  );
}
