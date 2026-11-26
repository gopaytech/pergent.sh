import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";
import { Link, useNavigate, useParams } from "react-router";

import { Button } from "@/components/ui/button";
import { Card, CardHeader, CardTitle } from "@/components/ui/card";
import { Label } from "@/components/ui/label";
import { Input } from "@/components/ui/input";
import { Textarea } from "@/components/ui/textarea";
import { cn } from "@/lib/utils";

import { api, isStatus, paths, type SkillDetail, type SkillVersion, type SkillVersionDetail } from "../api";
import { useMe } from "../layout/me";
import { useWrite } from "../layout/write";
import { Ago, ErrorBox, Loading, NotFound, PageHeader, Pill, SkillSourceText, bytesText } from "../ui";
import { useUnsavedGuard } from "../unsaved";
import Markdown from "../MarkdownLazy";

// maxSkillBytes is the most a skill may be (inputs.MaxSkillBytes, 128 KiB):
// the counter warns and Save waits; the server checks again.
const maxSkillBytes = 128 * 1024;

const sizeOf = (text: string) => new TextEncoder().encode(text).length;

// skillText is the skill text box: monospace, tall enough to read a skill.
const skillText = "min-h-[360px] font-mono text-xs";
// hint is the small grey line under a field.
const hint = "text-muted-foreground text-xs";

// Crumbs is the way back to the skill list, over the page's title.
function Crumbs({ here }: { here: string }) {
  return (
    <div className="text-muted-foreground mb-1.5 text-xs">
      <Link className="text-muted-foreground hover:underline" to="/skills">
        Skills
      </Link>{" "}
      / {here}
    </div>
  );
}

function SizeCounter({ text }: { text: string }) {
  const n = sizeOf(text);
  return (
    <span className={cn("text-xs", n > maxSkillBytes ? "problem text-bad" : "text-muted-foreground")}>
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
  const allow = useUnsavedGuard(name !== "" || content !== "");
  const create = useMutation({
    mutationFn: () => write("POST", "/api/admin/skills", { name: name.trim(), content, addToDefaults }),
    onSuccess: async () => {
      await client.invalidateQueries({ queryKey: ["skills"] });
      // The default list may have changed: repositories show it.
      void client.invalidateQueries({ queryKey: ["repositories"] });
      allow();
      navigate("/skills");
    },
  });
  const submit = (e: FormEvent) => {
    e.preventDefault();
    create.mutate();
  };
  return (
    <div className="max-w-[1100px]">
      <Crumbs here="New skill" />
      <PageHeader title="New skill" />
      <Card>
        <form className="flex flex-col gap-4 px-4" aria-label="New skill" onSubmit={submit}>
          <div className="grid gap-2">
            <Label htmlFor="skill-name">Name</Label>
            <Input
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
            <div className={hint}>The name can't change later.</div>
          </div>
          <div className="grid gap-2">
            <Label htmlFor="skill-text">Skill text</Label>
            <Textarea
              id="skill-text"
              className={skillText}
              spellCheck={false}
              value={content}
              onChange={(e) => setContent(e.target.value)}
              required
            />
            <div className={hint}>
              A markdown file. An optional frontmatter may set paths, exclude and cross_batch; its name must match the name above.
            </div>
          </div>
          <div className="grid gap-2">
            <Label className="font-normal">
              <input type="checkbox" className="accent-primary size-4" checked={addToDefaults} onChange={(e) => setAddToDefaults(e.target.checked)} />
              Add to default skills
            </Label>
            <div className={hint}>A repository that sets no skills of its own uses the default skills.</div>
          </div>
          <div className="flex flex-wrap items-center justify-end gap-3">
            <SizeCounter text={content} />
            <Button type="submit" disabled={create.isPending || sizeOf(content) > maxSkillBytes}>
              Create skill
            </Button>
          </div>
          {create.isError && <ErrorBox error={create.error} />}
        </form>
      </Card>
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
  const dirty = editable && draft !== base.content;
  useUnsavedGuard(dirty);
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
    <div className="flex max-w-[1100px] flex-col gap-4">
      <div>
        <Crumbs here={skill.name} />
        <PageHeader
          className="mb-2"
          title={
            <>
              {skill.name} {skill.inDefaults && <Pill tone="run">default</Pill>}
            </>
          }
          description={
            <>
              v{skill.version} · <SkillSourceText source={skill.source} />
              {skill.source?.commit ? ` · commit ${skill.source.commit.slice(0, 7)}` : ""} · {bytesText(skill.size)}
            </>
          }
        />
      </div>
      {editable ? (
        <Card>
          <form className="flex flex-col gap-3 px-4" aria-label="Edit skill" onSubmit={submit}>
            <Label htmlFor="skill-text">Skill text</Label>
            <Textarea id="skill-text" className={skillText} spellCheck={false} value={draft} onChange={(e) => setDraft(e.target.value)} />
            <div className="flex flex-wrap items-center justify-end gap-3">
              <SizeCounter text={draft} />
              {dirty && <span className={hint}>Unsaved changes</span>}
              <Button type="submit" disabled={save.isPending || draft === base.content || draft.trim() === "" || sizeOf(draft) > maxSkillBytes}>
                Save
              </Button>
            </div>
            {note && (
              <div className="text-ok text-sm" role="status">
                {note}
              </div>
            )}
            {save.isError && <ErrorBox error={save.error} />}
            {stale && (
              <div className="flex flex-wrap items-center gap-2">
                <Button type="button" variant="outline" size="sm" onClick={() => void reloadText()}>
                  Reload
                </Button>
                <span className={hint}>Reload shows the saved text and drops your changes.</span>
              </div>
            )}
          </form>
        </Card>
      ) : (
        // The markdown view brings its own room at the sides.
        <Card role="region" aria-label="Skill text">
          {me.admin && <div className="text-muted-foreground px-4 text-sm">An installed skill can't be edited. Copy it from the list to change it.</div>}
          <Markdown text={skill.content} />
        </Card>
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
    <Card role="region" aria-label="Versions" className="gap-0 py-0">
      <CardHeader className="border-b py-4">
        <CardTitle>Versions</CardTitle>
      </CardHeader>
      {versions.map((v) => (
        <div className="flex items-center gap-3 border-b px-4 py-3 text-sm last:border-b-0" key={v.version}>
          <b className="font-semibold">v{v.version}</b>
          <span className="text-muted-foreground min-w-0 grow text-xs">
            <Ago t={v.at} />
            {v.actor ? ` · ${v.actor}` : ""}
            {v.commit ? ` · commit ${v.commit.slice(0, 7)}` : ""}
          </span>
          <Button
            type="button"
            variant="outline"
            size="sm"
            aria-label={`View v${v.version}`}
            onClick={() => setShown(shown === v.version ? null : v.version)}
          >
            {shown === v.version ? "Hide" : "View"}
          </Button>
        </div>
      ))}
      {shown !== null && one.isPending && <Loading />}
      {shown !== null && one.isError && (
        <div className="px-4 py-3">
          <ErrorBox error={one.error} />
        </div>
      )}
      {shown !== null && one.isSuccess && (
        <div className="border-t pt-3">
          <Markdown text={one.data.content} />
        </div>
      )}
    </Card>
  );
}
