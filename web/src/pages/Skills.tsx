import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";
import { Link } from "react-router";

import { Alert } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";

import { ApiError, api, paths, type LibrarySkill, type SkillInUse } from "../api";
import { useMe } from "../layout/me";
import { useWrite } from "../layout/write";
import { ErrorBox, Loading, PageHeader, Pill, SkillSourceText, ago, bytesText } from "../ui";

// Skills lists pergent.sh's skill library. Everyone signed in reads it;
// admins also write it: New skill, Install from git, and per skill Edit
// (a written skill), Copy, Check for updates (an installed skill) and Delete.
// A skill's text is on its own page, shown as text.
export default function Skills() {
  const me = useMe();
  const skills = useQuery({ queryKey: ["skills"], queryFn: () => api.get<LibrarySkill[]>("/api/skills") });
  return (
    <div className="max-w-[1100px]">
      <PageHeader
        title="Skills"
        description="The skill library a repository's reviews can use. A repository's own skill of the same name replaces it."
        actions={
          me.admin && (
            <>
              <Button asChild variant="outline" size="sm">
                <Link to="/skills/install">Install from git</Link>
              </Button>
              <Button asChild size="sm">
                <Link to="/skills/new">New skill</Link>
              </Button>
            </>
          )
        }
      />
      {skills.isPending && <Loading />}
      {skills.isError && <ErrorBox error={skills.error} />}
      {skills.isSuccess && (
        <Card role="region" aria-label="Library skills" className="gap-0 py-0">
          {skills.data.length === 0 && <div className="text-muted-foreground px-4 py-3 text-sm">The library has no skills.</div>}
          {skills.data.map((s) => (
            <SkillRow key={s.name} skill={s} admin={me.admin} />
          ))}
        </Card>
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
    <div className="border-b text-sm last:border-b-0" data-skill={skill.name}>
      <div className="flex flex-wrap items-center gap-3 px-4 py-3">
        <div className="min-w-0 grow">
          <Link to={paths.skill(skill.name)} className="text-foreground font-semibold hover:underline">
            {skill.name}
          </Link>{" "}
          {skill.inDefaults && <Pill tone="run">default</Pill>}
          <div className="text-muted-foreground mt-0.5 text-xs">
            v{skill.version} · <SkillSourceText source={skill.source} /> · {bytesText(skill.size)} · changed {ago(skill.updatedAt)}
            {skill.updatedBy ? ` by ${skill.updatedBy}` : ""}
          </div>
        </div>
        {admin && (
          <span className="flex flex-wrap items-center gap-2">
            {skill.source === null ? (
              <Button asChild variant="outline" size="sm">
                <Link to={paths.skill(skill.name)}>Edit</Link>
              </Button>
            ) : (
              <Button asChild variant="outline" size="sm">
                <Link to={paths.skillUpdates(skill.name)}>Check for updates</Link>
              </Button>
            )}
            <Button type="button" variant="outline" size="sm" onClick={() => setCopying(!copying)}>
              Copy
            </Button>
            <Button type="button" variant="destructive-outline" size="sm" disabled={remove.isPending} onClick={confirmDelete}>
              Delete
            </Button>
          </span>
        )}
      </div>
      {copying && (
        <form className="flex items-end gap-3 px-4 pb-3" aria-label={`Copy ${skill.name}`} onSubmit={submitCopy}>
          <div className="grid min-w-0 grow gap-2">
            <Label htmlFor={`copy-${skill.name}`}>New name</Label>
            <Input
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
          <Button type="submit" disabled={copy.isPending}>
            Copy skill
          </Button>
        </form>
      )}
      {(copy.isError || remove.isError) && (
        <div className="px-4 pb-3">
          {copy.isError && <ErrorBox error={copy.error} />}
          {remove.isError && (inUse ? <InUseBox name={skill.name} inUse={inUse} /> : <ErrorBox error={remove.error} />)}
        </div>
      )}
    </div>
  );
}

// inUseBody is the 409 body of a delete of a skill that is in use, or null
// for any other failure.
function inUseBody(err: unknown): SkillInUse | null {
  if (!(err instanceof ApiError) || err.status !== 409) return null;
  const b = err.body as Partial<SkillInUse> | undefined;
  if (!b || !Array.isArray(b.repositories)) return null;
  return { error: err.message, repositories: b.repositories, inDefaults: b.inDefaults ?? false };
}

function InUseBox({ name, inUse }: { name: string; inUse: SkillInUse }) {
  return (
    // Alert is a grid with an icon column: block puts the text in the box.
    <Alert variant="destructive" className="block">
      {name} is in use. Take it off these first:
      <ul className="mt-1.5 list-disc pl-5">
        {inUse.repositories.map((r) => (
          <li key={`${r.instance}/${r.project}`}>{`${r.instance}: ${r.project}`}</li>
        ))}
        {inUse.inDefaults && <li>the default skills</li>}
      </ul>
    </Alert>
  );
}
