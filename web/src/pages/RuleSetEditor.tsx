import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";
import { Link, useNavigate, useParams } from "react-router";

import { api, isStatus, paths, type RuleSetDetail, type RuleSetWritten } from "../api";
import { useMe } from "../layout/me";
import { useWrite } from "../layout/write";
import { ErrorBox, Loading, NotFound, Pill, bytesText } from "../ui";
import { useUnsavedGuard } from "../unsaved";

// maxRuleSetBytes is the most a rule set may be (pergent's rule.MaxFileBytes,
// 64 KB): the counter warns and Save waits; the server checks again.
const maxRuleSetBytes = 64 * 1024;

const sizeOf = (text: string) => new TextEncoder().encode(text).length;

const formatHelp =
  "YAML: rules: a list of entries, each a path (a glob, as a skill's paths) and either a rule (the checklist, at most 16,000 characters) or off: true. The first entry that matches a file wins.";

const example = 'rules:\n  - path: "internal/api/**/*.go"\n    rule: |\n      Every handler that starts a transaction must defer tx.Rollback().\n';

function SizeCounter({ text }: { text: string }) {
  const n = sizeOf(text);
  return (
    <span className={n > maxRuleSetBytes ? "problem" : "meta"}>
      {bytesText(n)} of {bytesText(maxRuleSetBytes)}
    </span>
  );
}

// Notes are the server's notes on a saved text, such as an entry that hides
// a built-in rule.
function Notes({ notes }: { notes: string[] }) {
  if (notes.length === 0) return null;
  return (
    <ul className="note" aria-label="Notes">
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
    <div className="narrow">
      <div className="crumbs">
        <Link to="/rules">Rules</Link> / New rule set
      </div>
      <div className="top">
        <h1>New rule set</h1>
      </div>
      <form className="card pad stack" aria-label="New rule set" onSubmit={submit}>
        <div>
          <label htmlFor="rule-set-name">Name</label>
          <input
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
          <div className="meta">The name can't change later.</div>
        </div>
        <div>
          <label htmlFor="rule-set-text">Rules</label>
          <textarea id="rule-set-text" className="skill-text" spellCheck={false} value={content} onChange={(e) => setContent(e.target.value)} required />
          <div className="meta">{formatHelp}</div>
        </div>
        <div className="actions-row">
          <button type="submit" className="btn primary" disabled={create.isPending || sizeOf(content) > maxRuleSetBytes}>
            Create rule set
          </button>
          <SizeCounter text={content} />
        </div>
        {create.isError && <ErrorBox error={create.error} />}
      </form>
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
    <div className="narrow">
      <div className="crumbs">
        <Link to="/rules">Rules</Link> / {set.name}
      </div>
      <div className="top">
        <div>
          <h1>
            {set.name} {set.inDefaults && <Pill tone="run">default</Pill>} {config && <Pill tone="grey">config file</Pill>}
          </h1>
          <div className="sub">
            {set.entries} {set.entries === 1 ? "entry" : "entries"}
            {config ? "" : ` · v${set.version}`}
          </div>
        </div>
      </div>
      {editable ? (
        <form className="card pad stack" aria-label="Edit rule set" onSubmit={submit}>
          <label htmlFor="rule-set-text">Rules</label>
          <textarea id="rule-set-text" className="skill-text" spellCheck={false} value={draft} onChange={(e) => setDraft(e.target.value)} />
          <div className="meta">{formatHelp}</div>
          <div className="actions-row">
            <button
              type="submit"
              className="btn primary"
              disabled={save.isPending || draft === base.content || draft.trim() === "" || sizeOf(draft) > maxRuleSetBytes}
            >
              Save
            </button>
            <SizeCounter text={draft} />
            {dirty && <span className="meta">Unsaved changes</span>}
          </div>
          {saved && (
            <div className="note" role="status">
              Saved as v{saved.version}.
            </div>
          )}
          <Notes notes={saved ? saved.notes : set.notes} />
          {save.isError && <ErrorBox error={save.error} />}
        </form>
      ) : (
        <section className="card" aria-label="Rule set text">
          {config && <div className="pad meta">This rule set is defined in the config file. Change it there.</div>}
          <pre className="doc">{set.content}</pre>
          <Notes notes={set.notes} />
        </section>
      )}
    </div>
  );
}
