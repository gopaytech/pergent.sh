import { useState } from "react";

import type { BuiltInRule } from "./api";

// BuiltInRulePicker chooses which of pergent's built-in rules a repository
// gets: a box per rule, with its globs, a search by name or glob, and Select
// none. Each rule adds its text to the runs on its files, so the fewer the
// better. The choice keeps pergent's order (the order of rules); a search
// only hides rows, never a choice.
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
  const shown = q === "" ? rules : rules.filter((r) => r.name.toLowerCase().includes(q) || r.paths.some((p) => p.toLowerCase().includes(q)));
  const chosen = rules.filter((r) => selected.includes(r.name)).length;
  const toggle = (name: string, on: boolean) =>
    onChange(rules.map((r) => r.name).filter((n) => (n === name ? on : selected.includes(n))));
  return (
    <div role="group" aria-label="Choose built-in rules">
      <div className="pad built-in-bar">
        <input
          type="search"
          aria-label="Search built-in rules"
          placeholder="Search by name or glob, such as go or *.yaml"
          value={search}
          onChange={(e) => setSearch(e.target.value)}
        />
        <span className="meta">
          {chosen} of {rules.length} chosen
        </span>
        <button type="button" className="btn small" disabled={disabled || chosen === 0} onClick={() => onChange([])}>
          Select none
        </button>
      </div>
      {shown.length === 0 ? (
        <div className="pad meta">No built-in rule matches.</div>
      ) : (
        <ul className="built-in-pick" aria-label="Built-in rules">
          {shown.map((r) => (
            <li key={r.name} className="skill" data-built-in={r.name} aria-label={r.name}>
              <input
                type="checkbox"
                aria-label={`Built-in rule ${r.name}`}
                checked={selected.includes(r.name)}
                disabled={disabled}
                onChange={(e) => toggle(r.name, e.target.checked)}
              />
              <b>{r.name}</b>
              <span className="grow built-in-paths">
                {r.paths.map((p) => (
                  <code key={p}>{p}</code>
                ))}
              </span>
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}
