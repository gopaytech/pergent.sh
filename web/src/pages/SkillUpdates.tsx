import { useMutation, useQueryClient } from "@tanstack/react-query";
import { useQuery } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";
import { Link, useSearchParams } from "react-router";

import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";

import { api, isStatus, paths, type SkillDetail, type SkillFetch, type SkillFetchResult, type SkillUpdate } from "../api";
import { lineDiff } from "../diff";
import { useWrite } from "../layout/write";
import { FetchStatus, useSkillFetch, useStartFetch } from "../skillFetch";
import { ErrorBox, Loading, NotFound, PageHeader, Pill, SkillSourceText } from "../ui";

// hint is the small grey line under a field.
const hint = "text-muted-foreground text-xs";
// problem is a line that says why a skill can't be updated.
const problem = "problem text-bad px-4 pb-3 text-xs";

// sourceFolder is the folder a skill's file at path sits in: the folder a
// check for updates must read to find it again. A skill is folder/name.md or
// folder/name/SKILL.md ("" for the top of the repository).
export function sourceFolder(path: string | undefined): string {
  if (!path) return "";
  const parts = path.split("/");
  if (parts.pop() === "SKILL.md") parts.pop();
  return parts.join("/");
}

const removed = "removed at the source";
const renamed = "renamed at the source";

// SkillUpdates checks one installed skill's source for updates (admins). The
// skill is named in the query, ?skill=name. The check reads the source again,
// at the skill's ref or at a new branch or tag, and shows what would change
// for each skill installed from that source; nothing changes until Update.
export default function SkillUpdates() {
  const [params] = useSearchParams();
  const name = params.get("skill") ?? "";
  const skill = useQuery({
    queryKey: ["skill", name],
    queryFn: () => api.get<SkillDetail>(paths.apiSkill(name)),
    enabled: name !== "",
  });
  let body;
  if (name === "") {
    body = (
      <p className="text-muted-foreground text-sm">
        Choose a skill to check from the <Link to="/skills">Skills</Link> list.
      </p>
    );
  } else if (skill.isPending) {
    body = <Loading />;
  } else if (skill.isError) {
    return isStatus(skill.error, 404) ? <NotFound /> : <ErrorBox error={skill.error} />;
  } else if (skill.data.source === null) {
    body = <p className="text-muted-foreground text-sm">{name} was written in the dashboard, so it has no source to check.</p>;
  } else {
    body = <Check key={skill.data.name} skill={skill.data} />;
  }
  return (
    <div className="flex max-w-[1100px] flex-col gap-4">
      <div>
        <div className="text-muted-foreground mb-1.5 text-xs">
          <Link className="text-muted-foreground hover:underline" to="/skills">
            Skills
          </Link>{" "}
          / Check for updates
        </div>
        <PageHeader title="Check for updates" className="mb-2" />
      </div>
      {body}
    </div>
  );
}

function Check({ skill }: { skill: SkillDetail }) {
  const source = skill.source;
  const isURL = source?.kind === "url";
  const hidden = source?.hidden === true;
  const [repo, setRepo] = useState(source?.repo ?? "");
  const [ref, setRef] = useState(source?.ref ?? "");
  const [folder, setFolder] = useState(sourceFolder(source?.path));
  const [id, setId] = useState<string>();
  const start = useStartFetch(setId);
  const read = useSkillFetch(id);
  const running = start.isPending || read.data?.status === "running";
  const submit = (e: FormEvent) => {
    e.preventDefault();
    start.mutate({
      ...(isURL ? { url: source?.url } : { instance: source?.instance, repo: repo.trim() }),
      ref: ref.trim(),
      path: folder.trim(),
      purpose: "update",
      fromRef: source?.ref,
    });
  };
  return (
    <>
      <p className="text-muted-foreground text-sm">
        {skill.name} is installed from <SkillSourceText source={source} />. The check reads the source again and shows what would
        change for every skill installed from it, in the folder below. Nothing changes until you press Update.
      </p>
      <Card>
        <form className="flex flex-col gap-4 px-4" aria-label="Check a source" onSubmit={submit}>
          {hidden && (
            <div className="text-muted-foreground text-sm">
              This skill came from a private repository on {source?.instance}. Type the repository to check it.
            </div>
          )}
          {!isURL && (
            <div className="grid gap-2">
              <Label htmlFor="check-repo">Repository</Label>
              <Input
                id="check-repo"
                type="text"
                autoComplete="off"
                placeholder="group/project"
                value={repo}
                onChange={(e) => setRepo(e.target.value)}
                required
              />
            </div>
          )}
          <div className="grid gap-2">
            <Label htmlFor="check-ref">Branch or tag</Label>
            <Input id="check-ref" type="text" autoComplete="off" value={ref} onChange={(e) => setRef(e.target.value)} required />
            <div className={hint}>Type a newer branch or tag to move to it. Update then records it.</div>
          </div>
          <div className="grid gap-2">
            <Label htmlFor="check-folder">Folder</Label>
            <Input id="check-folder" type="text" autoComplete="off" value={folder} onChange={(e) => setFolder(e.target.value)} />
          </div>
          <div className="flex flex-wrap items-center gap-2">
            <Button type="submit" disabled={running || ref.trim() === "" || (!isURL && repo.trim() === "")}>
              Check for updates
            </Button>
          </div>
          {start.isError && <ErrorBox error={start.error} />}
        </form>
      </Card>
      {read.isPending && id !== undefined && <Loading />}
      {read.isError && <ErrorBox error={read.error} />}
      {read.isSuccess && read.data.status !== "done" && (
        <Card role="region" aria-label="Reading" className="px-4">
          <FetchStatus read={read.data} />
        </Card>
      )}
      {read.isSuccess && read.data.status === "done" && <Updates key={read.data.id} read={read.data} />}
    </>
  );
}

// canUpdate is whether Update may be pressed for u: not for a skill gone or
// renamed at the source or one with a problem, and for an unchanged one only
// when the ref moves (Update then records the new ref).
const canUpdate = (u: SkillUpdate, read: SkillFetch) =>
  u.state !== removed && u.state !== renamed && !u.problem && (u.state !== "same" || read.ref !== read.fromRef);

function Updates({ read }: { read: SkillFetch }) {
  const client = useQueryClient();
  const write = useWrite();
  const [results, setResults] = useState<Record<string, SkillFetchResult>>({});
  const update = useMutation({
    mutationFn: (names: string[]) => write<SkillFetchResult[]>("POST", paths.apiSkillFetchAction(read.id, "update"), { names }),
    onSuccess: async (rs) => {
      setResults((old) => ({
        ...old,
        ...Object.fromEntries(rs.map((r) => [r.name, r])),
      }));
      if (rs.some((r) => r.ok)) {
        // Not the read itself: once the new ref is recorded it no longer
        // finds these skills, and the diffs on the page stay as they were.
        await client.invalidateQueries({ queryKey: ["skills"] });
        await client.invalidateQueries({ queryKey: ["skill"] });
      }
    },
  });
  const open = read.updates.filter((u) => canUpdate(u, read) && results[u.name]?.ok !== true);
  return (
    <Card role="region" aria-label="Updates found" className="gap-0 py-0">
      <div className="text-muted-foreground border-b px-4 py-3 text-xs">
        {read.commit ? `Read commit ${read.commit.slice(0, 7)}.` : "Read."}
        {read.ref !== read.fromRef && read.fromRef ? ` Update moves these skills from ${read.fromRef} to ${read.ref}.` : ""}
      </div>
      {read.updates.length === 0 && (
        <div className="text-muted-foreground px-4 py-3 text-sm">No skill was installed from this source and folder at {read.fromRef || read.ref}.</div>
      )}
      {read.updates.map((u) => (
        <UpdateRow
          key={u.path}
          u={u}
          result={results[u.name]}
          allowed={canUpdate(u, read)}
          busy={update.isPending}
          onUpdate={() => update.mutate([u.name])}
        />
      ))}
      {open.length > 1 && (
        <div className="flex flex-wrap items-center gap-2 px-4 py-3">
          <Button type="button" variant="outline" size="sm" disabled={update.isPending} onClick={() => update.mutate(open.map((u) => u.name))}>
            Update all
          </Button>
        </div>
      )}
      {update.isError && (
        <div className="px-4 py-3">
          <ErrorBox error={update.error} />
        </div>
      )}
    </Card>
  );
}

function UpdateRow({
  u,
  result,
  allowed,
  busy,
  onUpdate,
}: {
  u: SkillUpdate;
  result: SkillFetchResult | undefined;
  allowed: boolean;
  busy: boolean;
  onUpdate: () => void;
}) {
  const done = result?.ok === true;
  return (
    <div className="border-b text-sm last:border-b-0" data-update={u.name}>
      <div className="flex flex-wrap items-center gap-3 px-4 py-3">
        <div className="min-w-0 grow">
          <h3 className="text-sm font-semibold">{u.name}</h3>
          <div className={hint}>
            v{u.version} · {u.path}
          </div>
        </div>
        <StateText u={u} />
        <Button type="button" variant="outline" size="sm" disabled={!allowed || done || busy} onClick={onUpdate}>
          Update
        </Button>
      </div>
      {u.problem && <div className={problem}>{u.problem}</div>}
      {u.state === renamed && (
        <div className="text-muted-foreground px-4 pb-3 text-xs">The file now holds a skill with another name. Install it again from Install from git.</div>
      )}
      {done && (
        <div className="px-4 pb-3">
          <Pill tone="ok">Updated</Pill>
        </div>
      )}
      {result && !result.ok && <div className={problem}>{result.error}</div>}
      {u.state === "changed" && !u.problem && <Diff current={u.current} fetched={u.fetched} />}
    </div>
  );
}

function StateText({ u }: { u: SkillUpdate }) {
  switch (u.state) {
    case "changed":
      return <Pill tone="run">Changed</Pill>;
    case "same":
      return <span className={hint}>No change</span>;
    default:
      return <Pill tone="warn">{u.state}</Pill>;
  }
}

const marks = { same: "  ", add: "+ ", del: "- " };

// Diff shows what an update changes as text, one block per line, marked "+" for
// an added line and "-" for a removed one. "doc diff" and "diff-<kind>" stay as
// class names: they draw the block and colour the lines.
function Diff({ current, fetched }: { current: string; fetched: string }) {
  return (
    <pre className="doc diff" role="group" aria-label="Changes">
      {lineDiff(current, fetched).map((l, i) => (
        <span key={i} className={`diff-${l.kind}`}>
          {marks[l.kind] + l.text}
        </span>
      ))}
    </pre>
  );
}
