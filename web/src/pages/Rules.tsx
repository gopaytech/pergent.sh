import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import { Link } from "react-router";

import { ApiError, api, paths, type BuiltInRule, type RuleSet, type SkillInUse } from "../api";
import { useMe } from "../layout/me";
import { useWrite } from "../layout/write";
import { ErrorBox, Loading, Pill, Tabs, ago } from "../ui";

// Rules lists the rule library: named rule sets of path rules, which a
// repository chooses and orders on its Settings tab, and pergent's built-in
// rules (read-only). Everyone signed in reads it; admins write the sets. A
// set of the config file is read-only here.
export default function Rules() {
  const me = useMe();
  const [tab, setTab] = useState<"sets" | "builtIn">("sets");
  return (
    <div className="narrow">
      <div className="top">
        <div>
          <h1>Rules</h1>
          <div className="sub">
            Checklists for kinds of files, added to the review that already runs on them. A rule never says what is blocking.
          </div>
        </div>
        {me.admin && tab === "sets" && (
          <div className="actions-row">
            <Link to="/rules/new" className="btn primary">
              New rule set
            </Link>
          </div>
        )}
      </div>
      <Tabs<"sets" | "builtIn">
        tabs={[
          ["sets", "Rule sets"],
          ["builtIn", "Built-in"],
        ]}
        value={tab}
        onChange={setTab}
      />
      <div role="tabpanel">{tab === "sets" ? <RuleSetList admin={me.admin} /> : <BuiltInList />}</div>
    </div>
  );
}

function RuleSetList({ admin }: { admin: boolean }) {
  const sets = useQuery({ queryKey: ["rule-sets"], queryFn: () => api.get<RuleSet[]>("/api/rule-sets") });
  if (sets.isPending) return <Loading />;
  if (sets.isError) return <ErrorBox error={sets.error} />;
  return (
    <section className="card" aria-label="Rule sets">
      {sets.data.length === 0 && <div className="row empty-row">The library has no rule sets.</div>}
      {sets.data.map((s) => (
        <RuleSetRow key={s.name} set={s} admin={admin} />
      ))}
    </section>
  );
}

function RuleSetRow({ set, admin }: { set: RuleSet; admin: boolean }) {
  const client = useQueryClient();
  const write = useWrite();
  const remove = useMutation({
    mutationFn: () => write<void>("DELETE", paths.apiAdminRuleSet(set.name)),
    onSuccess: () => client.invalidateQueries({ queryKey: ["rule-sets"] }),
  });
  const confirmDelete = () => {
    if (window.confirm(`Delete the rule set ${set.name}?`)) remove.mutate();
  };
  const inUse = remove.isError ? inUseBody(remove.error) : null;
  const config = set.source === "config";
  return (
    <div className="skill-row" data-rule-set={set.name}>
      <div className="skill">
        <div className="grow">
          <Link to={paths.ruleSet(set.name)} className="skill-name">
            {set.name}
          </Link>{" "}
          {set.inDefaults && <Pill tone="run">default</Pill>} {config && <Pill tone="grey">config file</Pill>}
          <div className="meta">
            {set.entries} {set.entries === 1 ? "entry" : "entries"}
            {config ? " · read-only: change it in the config file" : ` · v${set.version} · changed ${ago(set.updatedAt)}`}
          </div>
        </div>
        {admin && !config && (
          <span className="actions-row">
            <Link to={paths.ruleSet(set.name)} className="btn small">
              Edit
            </Link>
            <button type="button" className="btn small" disabled={remove.isPending} onClick={confirmDelete}>
              Delete
            </button>
          </span>
        )}
      </div>
      {remove.isError && (inUse ? <InUseBox name={set.name} inUse={inUse} /> : <ErrorBox error={remove.error} />)}
    </div>
  );
}

function BuiltInList() {
  const rules = useQuery({ queryKey: ["built-in-rules"], queryFn: () => api.get<BuiltInRule[]>("/api/built-in-rules") });
  if (rules.isPending) return <Loading />;
  if (rules.isError) return <ErrorBox error={rules.error} />;
  return (
    <section className="card" aria-label="Built-in rules">
      <div className="row meta">
        pergent's own rules, after the repository's and the rule sets'. Copy one into a rule set to change it. Turn them off with
        Built-in rules on a repository's Settings, or with rules: false in a skill.
      </div>
      {rules.data.length === 0 && <div className="row empty-row">No built-in rules yet.</div>}
      {rules.data.map((r) => (
        <div className="row" key={`${r.path}|${r.name}`} data-built-in={r.name}>
          <div>
            <code>{r.path}</code> {r.off ? <span className="meta">off: no rule</span> : <strong>{r.name}</strong>}
          </div>
          {!r.off && <pre className="skill-text">{r.rule}</pre>}
        </div>
      ))}
    </section>
  );
}

// inUseBody is the 409 body of a delete of a rule set that is in use, or null
// for any other failure.
function inUseBody(err: unknown): SkillInUse | null {
  if (!(err instanceof ApiError) || err.status !== 409) return null;
  const b = err.body as Partial<SkillInUse> | undefined;
  if (!b || !Array.isArray(b.repositories)) return null;
  return { error: err.message, repositories: b.repositories, inDefaults: b.inDefaults ?? false };
}

function InUseBox({ name, inUse }: { name: string; inUse: SkillInUse }) {
  return (
    <div className="error" role="alert">
      {name} is in use. Take it off these first:
      <ul>
        {inUse.repositories.map((r) => (
          <li key={`${r.instance}/${r.project}`}>{`${r.instance}: ${r.project}`}</li>
        ))}
        {inUse.inDefaults && <li>the default rule sets</li>}
      </ul>
    </div>
  );
}
