import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";
import { Link, useNavigate, useParams } from "react-router";

import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Textarea } from "@/components/ui/textarea";
import { cn } from "@/lib/utils";

import { api, isStatus, paths, type RuleSetDetail, type RuleSetWritten } from "../api";
import { useMe } from "../layout/me";
import { useWrite } from "../layout/write";
import { ErrorBox, Loading, NotFound, PageHeader, Pill, bytesText } from "../ui";
import { useUnsavedGuard } from "../unsaved";

// maxRuleSetBytes is the most a rule set may be (pergent's rule.MaxFileBytes,
// 64 KB): the counter warns and Save waits; the server checks again.
const maxRuleSetBytes = 64 * 1024;

const sizeOf = (text: string) => new TextEncoder().encode(text).length;

const formatHelp =
  "YAML: rules: a list of entries, each a path (a glob, as a skill's paths) and either a rule (the checklist, at most 16,000 characters) or off: true. The first entry that matches a file wins.";

const example = 'rules:\n  - path: "internal/api/**/*.go"\n    rule: |\n      Every handler that starts a transaction must defer tx.Rollback().\n';

// ruleText is the rules text box: monospace, tall enough to read a set.
const ruleText = "min-h-[360px] font-mono text-xs";
// hint is the small grey line under a field.
const hint = "text-muted-foreground text-xs";

// Crumbs is the way back to the rule list, over the page's title.
function Crumbs({ here }: { here: string }) {
  return (
    <div className="text-muted-foreground mb-1.5 text-xs">
      <Link className="text-muted-foreground hover:underline" to="/rules">
        Rules
      </Link>{" "}
      / {here}
    </div>
  );
}

function SizeCounter({ text }: { text: string }) {
  const n = sizeOf(text);
  return (
    <span className={cn("text-xs", n > maxRuleSetBytes ? "problem text-bad" : "text-muted-foreground")}>
      {bytesText(n)} of {bytesText(maxRuleSetBytes)}
    </span>
  );
}

// Notes are the server's notes on a saved text, such as an entry that hides
// a built-in rule.
function Notes({ notes }: { notes: string[] }) {
  if (notes.length === 0) return null;
  return (
    <ul className="text-ok list-disc pl-5 text-sm" aria-label="Notes">
      {notes.map((n) => (
        <li key={n}>{n}</li>
      ))}
    </ul>
  );
}

// NewRuleSet is the form that writes a rule set into the library (admins).
export function NewRuleSet() {
  const client = useQueryClient();
  const write = useWrite();
  const navigate = useNavigate();
  const [name, setName] = useState("");
  const [content, setContent] = useState(example);
  const allow = useUnsavedGuard(name !== "" || content !== example);
  const create = useMutation({
    mutationFn: () => write<RuleSetWritten>("POST", "/api/admin/rule-sets", { name: name.trim(), content }),
    onSuccess: async () => {
      await client.invalidateQueries({ queryKey: ["rule-sets"] });
      allow();
      navigate(paths.ruleSet(name.trim()));
    },
  });
  const submit = (e: FormEvent) => {
    e.preventDefault();
    create.mutate();
  };
  return (
    <div className="max-w-[1100px]">
      <Crumbs here="New rule set" />
      <PageHeader title="New rule set" />
      <Card>
        <form className="flex flex-col gap-4 px-4" aria-label="New rule set" onSubmit={submit}>
          <div className="grid gap-2">
            <Label htmlFor="rule-set-name">Name</Label>
            <Input
              id="rule-set-name"
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
            <Label htmlFor="rule-set-text">Rules</Label>
            <Textarea id="rule-set-text" className={ruleText} spellCheck={false} value={content} onChange={(e) => setContent(e.target.value)} required />
            <div className={hint}>{formatHelp}</div>
          </div>
          <div className="flex flex-wrap items-center justify-end gap-3">
            <SizeCounter text={content} />
            <Button type="submit" disabled={create.isPending || sizeOf(content) > maxRuleSetBytes}>
              Create rule set
            </Button>
          </div>
          {create.isError && <ErrorBox error={create.error} />}
        </form>
      </Card>
    </div>
  );
}

// RuleSetEditor is one rule set: its text for everyone, and for an admin the
// editor, unless the set is the config file's.
export default function RuleSetEditor() {
  const { name = "" } = useParams();
  const set = useQuery({ queryKey: ["rule-set", name], queryFn: () => api.get<RuleSetDetail>(paths.apiRuleSet(name)) });
  if (set.isPending) return <Loading />;
  if (set.isError) return isStatus(set.error, 404) ? <NotFound /> : <ErrorBox error={set.error} />;
  return <RuleSetPage key={set.data.name} set={set.data} />;
}

function RuleSetPage({ set }: { set: RuleSetDetail }) {
  const me = useMe();
  const client = useQueryClient();
  const write = useWrite();
  // base is the version the draft was opened from: Save names it, so a change
  // made since is refused as stale, not overwritten.
  const [base, setBase] = useState({ version: set.version, content: set.content });
  const [draft, setDraft] = useState(set.content);
  const [saved, setSaved] = useState<RuleSetWritten | null>(null);
  const dirty = draft !== base.content;
  useUnsavedGuard(dirty);
  const config = set.source === "config";
  const editable = me.admin && !config;
  const save = useMutation({
    mutationFn: (body: { content: string; version: number }) => write<RuleSetWritten>("PUT", paths.apiAdminRuleSet(set.name), body),
    onMutate: () => setSaved(null),
    onSuccess: async (r, body) => {
      setBase({ version: r.version, content: body.content });
      setSaved(r);
      await client.invalidateQueries({ queryKey: ["rule-set", set.name] });
      void client.invalidateQueries({ queryKey: ["rule-sets"] });
    },
  });
  const submit = (e: FormEvent) => {
    e.preventDefault();
    save.mutate({ content: draft, version: base.version });
  };
  return (
    <div className="max-w-[1100px]">
      <Crumbs here={set.name} />
      <PageHeader
        title={
          <>
            {set.name} {set.inDefaults && <Pill tone="run">default</Pill>} {config && <Pill tone="grey">config file</Pill>}
          </>
        }
        description={
          <>
            {set.entries} {set.entries === 1 ? "entry" : "entries"}
            {config ? "" : ` · v${set.version}`}
          </>
        }
      />
      {editable ? (
        <Card>
          <form className="flex flex-col gap-3 px-4" aria-label="Edit rule set" onSubmit={submit}>
            <Label htmlFor="rule-set-text">Rules</Label>
            <Textarea id="rule-set-text" className={ruleText} spellCheck={false} value={draft} onChange={(e) => setDraft(e.target.value)} />
            <div className={hint}>{formatHelp}</div>
            <div className="flex flex-wrap items-center justify-end gap-3">
              <SizeCounter text={draft} />
              {dirty && <span className={hint}>Unsaved changes</span>}
              <Button type="submit" disabled={save.isPending || draft === base.content || draft.trim() === "" || sizeOf(draft) > maxRuleSetBytes}>
                Save
              </Button>
            </div>
            {saved && (
              <div className="text-ok text-sm" role="status">
                Saved as v{saved.version}.
              </div>
            )}
            <Notes notes={saved ? saved.notes : set.notes} />
            {save.isError && <ErrorBox error={save.error} />}
          </form>
        </Card>
      ) : (
        <Card role="region" aria-label="Rule set text" className="px-4">
          {config && <div className="text-muted-foreground text-sm">This rule set is defined in the config file. Change it there.</div>}
          {/* "doc" stays: it draws the text as a code block. */}
          <pre className="doc m-0 overflow-auto">{set.content}</pre>
          <Notes notes={set.notes} />
        </Card>
      )}
    </div>
  );
}
