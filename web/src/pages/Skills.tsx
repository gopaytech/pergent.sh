import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";
import { Link } from "react-router";

import { ApiError, api, paths, type LibrarySkill, type SkillInUse } from "../api";
import { useMe } from "../layout/me";
import { useWrite } from "../layout/write";
import { ErrorBox, Loading, Pill, SkillSourceText, ago, bytesText } from "../ui";

// Skills lists pergent.sh's skill library. Everyone signed in reads it;
// admins also write it: New skill, Install from git, and per skill Edit
// (a written skill), Copy, Check for updates (an installed skill) and Delete.
// A skill's text is on its own page, shown as text.
export default function Skills() {
  const me = useMe();
  const skills = useQuery({ queryKey: ["skills"], queryFn: () => api.get<LibrarySkill[]>("/api/skills") });
  return (
    <div className="narrow">
      <div className="top">
        <div>
          <h1>Skills</h1>
          <div className="sub">The skill library a repository's reviews can use. A repository's own skill of the same name replaces it.</div>
        </div>
        {me.admin && (
          <div className="actions-row">
            <Link to="/skills/install" className="btn">
              Install from git
            </Link>
            <Link to="/skills/new" className="btn primary">
              New skill
            </Link>
          </div>
        )}
      </div>
      {skills.isPending && <Loading />}
      {skills.isError && <ErrorBox error={skills.error} />}
      {skills.isSuccess && (
        <section className="card" aria-label="Library skills">
          {skills.data.length === 0 && <div className="row empty-row">The library has no skills.</div>}
          {skills.data.map((s) => (
            <SkillRow key={s.name} skill={s} admin={me.admin} />
          ))}
        </section>
      )}
    </div>
  );
}

function SkillRow({ skill, admin }: { skill: LibrarySkill; admin: boolean }) {
  const client = useQueryClient();
  const write = useWrite();
  const [copying, setCopying] = useState(false);
  const [newName, setNewName] = useState("");
  const copy = useMutation({
    mutationFn: () => write("POST", paths.apiAdminSkillCopy(skill.name), { name: newName.trim() }),
    onSuccess: () => {
      setCopying(false);
      setNewName("");
      return client.invalidateQueries({ queryKey: ["skills"] });
    },
  });
  const remove = useMutation({
    mutationFn: () => write<void>("DELETE", paths.apiAdminSkill(skill.name)),
    onSuccess: () => client.invalidateQueries({ queryKey: ["skills"] }),
  });
  const submitCopy = (e: FormEvent) => {
    e.preventDefault();
    copy.mutate();
  };
  const confirmDelete = () => {
    if (window.confirm(`Delete the skill ${skill.name}? Its versions stay in the history.`)) remove.mutate();
  };
  const inUse = remove.isError ? inUseBody(remove.error) : null;
  return (
    <div className="skill-row" data-skill={skill.name}>
      <div className="skill">
        <div className="grow">
          <Link to={paths.skill(skill.name)} className="skill-name">
            {skill.name}
          </Link>{" "}
          {skill.inDefaults && <Pill tone="run">default</Pill>}
          <div className="meta">
            v{skill.version} · <SkillSourceText source={skill.source} /> · {bytesText(skill.size)} · changed {ago(skill.updatedAt)}
            {skill.updatedBy ? ` by ${skill.updatedBy}` : ""}
          </div>
        </div>
        {admin && (
          <span className="actions-row">
            {skill.source === null ? (
              <Link to={paths.skill(skill.name)} className="btn small">
                Edit
              </Link>
            ) : (
              <Link to="/skills/updates" className="btn small">
                Check for updates
              </Link>
            )}
            <button type="button" className="btn small" onClick={() => setCopying(!copying)}>
              Copy
            </button>
            <button type="button" className="btn small" disabled={remove.isPending} onClick={confirmDelete}>
              Delete
            </button>
          </span>
        )}
      </div>
      {copying && (
        <form className="pad inline-form" aria-label={`Copy ${skill.name}`} onSubmit={submitCopy}>
          <div className="grow">
            <label htmlFor={`copy-${skill.name}`}>New name</label>
            <input
              id={`copy-${skill.name}`}
              type="text"
              autoComplete="off"
              maxLength={100}
              pattern="[A-Za-z0-9][A-Za-z0-9._\-]*"
              title="Letters, digits, dots, dashes and underscores, starting with a letter or digit"
              value={newName}
              onChange={(e) => setNewName(e.target.value)}
              required
            />
          </div>
          <button type="submit" className="btn primary" disabled={copy.isPending}>
            Copy skill
          </button>
        </form>
      )}
      {copy.isError && <ErrorBox error={copy.error} />}
      {remove.isError && (inUse ? <InUseBox name={skill.name} inUse={inUse} /> : <ErrorBox error={remove.error} />)}
    </div>
  );
}

// inUseBody is the 409 body of a delete of a skill that is in use, or null
// for any other failure.
function inUseBody(err: unknown): SkillInUse | null {
  if (!(err instanceof ApiError) || err.status !== 409) return null;
  const b = err.body as Partial<SkillInUse> | undefined;
  if (!b || !Array.isArray(b.repositories)) return null;
  return { error: err.message, repositories: b.repositories, hidden: b.hidden ?? 0, inDefaults: b.inDefaults ?? false };
}

// hiddenText counts the repositories that use a skill and that the admin
// can't see.
const hiddenText = (n: number) => `+${n} ${n === 1 ? "repository" : "repositories"} you can't see`;

function InUseBox({ name, inUse }: { name: string; inUse: SkillInUse }) {
  return (
    <div className="error" role="alert">
      {name} is in use. Take it off these first:
      <ul>
        {inUse.repositories.map((r) => (
          <li key={`${r.instance}/${r.project}`}>{`${r.instance}: ${r.project}`}</li>
        ))}
        {inUse.inDefaults && <li>the default skills</li>}
        {inUse.hidden > 0 && <li>{hiddenText(inUse.hidden)}</li>}
      </ul>
    </div>
  );
}
