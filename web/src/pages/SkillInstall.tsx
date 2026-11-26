import { useMutation, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";
import { Link } from "react-router";

import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";

import { paths, type FetchedSkill, type SkillFetchResult } from "../api";
import { useWrite } from "../layout/write";
import { FetchStatus, useSkillFetch, useStartFetch } from "../skillFetch";
import { ErrorBox, Loading, PageHeader, Pill, bytesText } from "../ui";

// hint is the small grey line under a field.
const hint = "text-muted-foreground text-xs";
// tick is a plain tick box or radio: there is no shadcn one on this page.
const tick = "accent-primary size-4";
// field is a label, its box and its hint.
const field = "grid gap-2";

// SkillInstall installs skills into the library from a git source (admins):
// a repository of a connected instance, or a public URL. It reads the source
// first, lists the skills found, and installs the ticked ones.
export default function SkillInstall() {
  const [kind, setKind] = useState<"instance" | "url">("instance");
  const [instance, setInstance] = useState("");
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
    <div className="flex max-w-[1100px] flex-col gap-4">
      <div>
        <div className="text-muted-foreground mb-1.5 text-xs">
          <Link className="text-muted-foreground hover:underline" to="/skills">
            Skills
          </Link>{" "}
          / Install from git
        </div>
        <PageHeader title="Install from git" className="mb-2" />
      </div>
      <p className="text-muted-foreground text-sm">
        Installed skills are readable by every signed-in user, including skills from a private repository. Install only what
        everyone may read.
      </p>
      <Card>
        <form className="flex flex-col gap-4 px-4" aria-label="Read a source" onSubmit={submit}>
          <div className="flex flex-wrap items-center gap-4">
            <Label className="font-normal">
              <input type="radio" className={tick} name="source" checked={kind === "instance"} onChange={() => setKind("instance")} />A
              repository on an instance
            </Label>
            <Label className="font-normal">
              <input type="radio" className={tick} name="source" checked={kind === "url"} onChange={() => setKind("url")} />A public URL
            </Label>
          </div>
          {kind === "instance" ? (
            <>
              <div className={field}>
                <Label htmlFor="fetch-instance">Instance</Label>
                <Input
                  id="fetch-instance"
                  type="text"
                  autoComplete="off"
                  value={instance}
                  onChange={(e) => setInstance(e.target.value)}
                  required
                />
              </div>
              <div className={field}>
                <Label htmlFor="fetch-repo">Repository</Label>
                <Input
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
            <div className={field}>
              <Label htmlFor="fetch-url">URL</Label>
              <Input
                id="fetch-url"
                type="text"
                autoComplete="off"
                placeholder="https://example.com/skills.git"
                value={url}
                onChange={(e) => setUrl(e.target.value)}
                required
              />
              <div className={hint}>
                A public https git URL, read in a Job that holds no token. The Job reaches only the hosts on its network allow list
                (the Helm value jobEgress.cidrs), so the git host must be on it.
              </div>
            </div>
          )}
          <div className={field}>
            <Label htmlFor="fetch-ref">Branch or tag</Label>
            <Input
              id="fetch-ref"
              type="text"
              autoComplete="off"
              placeholder="main or v1.0"
              value={ref}
              onChange={(e) => setRef(e.target.value)}
              required
            />
          </div>
          <div className={field}>
            <Label htmlFor="fetch-folder">Folder</Label>
            <Input
              id="fetch-folder"
              type="text"
              autoComplete="off"
              placeholder="skills"
              value={folder}
              onChange={(e) => setFolder(e.target.value)}
            />
            <div className={hint}>Leave it empty to read the top of the repository. Each skill is name.md or name/SKILL.md.</div>
          </div>
          <div className="flex flex-wrap items-center gap-2">
            <Button type="submit" disabled={running}>
              Read skills
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
    <Card role="region" aria-label="Skills found" className="gap-0 py-0">
      <form onSubmit={submit}>
        <div className="text-muted-foreground border-b px-4 py-3 text-xs">{commit ? `Read commit ${commit.slice(0, 7)}.` : "Read."}</div>
        {skills.length === 0 && <div className="text-muted-foreground border-b px-4 py-3 text-sm">No skills found in this folder.</div>}
        {skills.map((s) => {
          const result = results[s.name];
          const installed = result?.ok === true;
          const blocked = s.taken || !!s.problem || installed;
          return (
            <div className="flex items-start gap-2.5 border-b px-4 py-3 text-sm" key={s.path}>
              <input
                type="checkbox"
                className={`${tick} mt-0.5`}
                aria-label={s.name}
                disabled={blocked}
                checked={ticked.includes(s.name)}
                onChange={(e) => toggle(s.name, e.target.checked)}
              />
              <div className="min-w-0 grow">
                <b className="font-semibold">{s.name}</b> <span className={hint}>{s.path}</span>
                {s.size > 0 && <span className={hint}> · {bytesText(s.size)}</span>}
                {s.problem && <div className="problem text-bad text-xs">{s.problem}</div>}
                {s.taken && !installed && <div className={hint}>name already in the library</div>}
                {installed && <Pill tone="ok">Installed</Pill>}
                {result && !result.ok && <div className="problem text-bad text-xs">{result.error}</div>}
              </div>
            </div>
          );
        })}
        <div className="flex flex-col gap-3 px-4 py-4">
          <div className="grid gap-2">
            <Label className="font-normal">
              <input type="checkbox" className={tick} checked={addToDefaults} onChange={(e) => setAddToDefaults(e.target.checked)} />
              Add to default skills
            </Label>
            <div className={hint}>A repository that sets no skills of its own uses the default skills.</div>
          </div>
          <div className="flex flex-wrap items-center gap-2">
            <Button type="submit" disabled={install.isPending || ticked.length === 0}>
              Install
            </Button>
          </div>
          {install.isError && <ErrorBox error={install.error} />}
        </div>
      </form>
    </Card>
  );
}
