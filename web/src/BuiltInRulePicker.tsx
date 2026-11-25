import { useState } from "react";

import type { BuiltInRule } from "./api";
import Markdown from "./MarkdownLazy";

// BuiltInRulePicker enables pergent's built-in rules for a repository. The
// enabled rules are pinned on top, where a search never hides them; below
// are a search by name or glob and the rules not enabled. Ticking a rule
// moves it up, unticking moves it back. Each row opens to its globs and text.
// Each rule adds its text to the runs on its files, so the fewer the better.
// The list keeps pergent's order (the order of rules).
export default function BuiltInRulePicker({
  rules,
  selected,
  disabled,
  onChange,
}: {
  rules: BuiltInRule[];
  selected: string[];
  disabled: boolean;
  onChange: (names: string[]) => void;
}) {
  const [search, setSearch] = useState("");
  const q = search.trim().toLowerCase();
  const on = rules.filter((r) => selected.includes(r.name));
  const rest = rules.filter(
    (r) => !selected.includes(r.name) && (q === "" || r.name.toLowerCase().includes(q) || r.paths.some((p) => p.toLowerCase().includes(q))),
  );
  const toggle = (name: string, tick: boolean) =>
    onChange(rules.map((r) => r.name).filter((n) => (n === name ? tick : selected.includes(n))));
  const row = (r: BuiltInRule) => (
    <li key={r.name} className="skill" data-built-in={r.name} aria-label={r.name}>
      <input
        type="checkbox"
        aria-label={`Built-in rule ${r.name}`}
        checked={selected.includes(r.name)}
        disabled={disabled}
        onChange={(e) => toggle(r.name, e.target.checked)}
      />
      <details className="grow built-in-doc">
        <summary>
          <b>{r.name}</b>
          <span className="grow built-in-paths">
            {r.paths.map((p) => (
              <code key={p}>{p}</code>
            ))}
          </span>
        </summary>
        <Markdown text={r.rule} />
      </details>
    </li>
  );
  return (
    <div role="group" aria-label="Enable built-in rules">
      <div className="pad built-in-bar">
        <h3 className="group grow">Enabled ({on.length})</h3>
        <span className="meta">
          {on.length} of {rules.length} enabled
        </span>
        <button type="button" className="btn small" disabled={disabled || on.length === 0} onClick={() => onChange([])}>
          Select none
        </button>
      </div>
      <ul className="built-in-pick" aria-label="Enabled built-in rules">
        {on.map(row)}
      </ul>
      {on.length === 0 && <div className="pad meta">No built-in rule is enabled.</div>}
      <div className="pad built-in-bar">
        <input
          type="search"
          aria-label="Search built-in rules"
          placeholder="Search by name or glob, such as go or *.yaml"
          value={search}
          onChange={(e) => setSearch(e.target.value)}
        />
      </div>
      {rest.length === 0 ? (
        <div className="pad meta">{q === "" ? "Every built-in rule is enabled." : "No other built-in rule matches."}</div>
      ) : (
        <ul className="built-in-pick" aria-label="Built-in rules not enabled">
          {rest.map(row)}
        </ul>
      )}
    </div>
  );
}
