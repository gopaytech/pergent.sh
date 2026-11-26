import { useState } from "react";

import { ChevronRight } from "lucide-react";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";

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
    <li key={r.name} className="flex items-start gap-2.5 border-b px-4 py-2.5 text-sm last:border-b-0" data-built-in={r.name} aria-label={r.name}>
      <input
        type="checkbox"
        className="accent-primary mt-0.5 size-4 shrink-0"
        aria-label={`Built-in rule ${r.name}`}
        checked={selected.includes(r.name)}
        disabled={disabled}
        onChange={(e) => toggle(r.name, e.target.checked)}
      />
      <details className="group min-w-0 grow">
        <summary className="flex cursor-pointer list-none items-center gap-2.5 [&::-webkit-details-marker]:hidden">
          <ChevronRight className="text-muted-foreground size-4 shrink-0 transition-transform group-open:rotate-90" aria-hidden="true" />
          <b className="font-semibold">{r.name}</b>
          <span className="flex min-w-0 grow flex-wrap gap-x-2 gap-y-1">
            {r.paths.map((p) => (
              <code key={p} className="text-muted-foreground font-mono text-xs">
                {p}
              </code>
            ))}
          </span>
        </summary>
        <div className="mt-2">
          <Markdown text={r.rule} />
        </div>
      </details>
    </li>
  );
  return (
    <div role="group" aria-label="Enable built-in rules">
      <div className="flex items-center gap-2.5 border-b px-4 py-2.5">
        <h3 className="text-muted-foreground grow text-[13px] font-semibold">Enabled ({on.length})</h3>
        <span className="text-muted-foreground text-sm whitespace-nowrap">
          {on.length} of {rules.length} enabled
        </span>
        <Button type="button" variant="outline" size="sm" disabled={disabled || on.length === 0} onClick={() => onChange([])}>
          Select none
        </Button>
      </div>
      <ul className="max-h-90 overflow-y-auto border-b empty:hidden" aria-label="Enabled built-in rules">
        {on.map(row)}
      </ul>
      {on.length === 0 && <div className="text-muted-foreground border-b px-4 py-2.5 text-sm">No built-in rule is enabled.</div>}
      <div className="border-b px-4 py-2.5">
        <Input
          type="search"
          aria-label="Search built-in rules"
          placeholder="Search by name or glob, such as go or *.yaml"
          value={search}
          onChange={(e) => setSearch(e.target.value)}
        />
      </div>
      {rest.length === 0 ? (
        <div className="text-muted-foreground px-4 py-2.5 text-sm">{q === "" ? "Every built-in rule is enabled." : "No other built-in rule matches."}</div>
      ) : (
        <ul className="max-h-90 overflow-y-auto" aria-label="Built-in rules not enabled">
          {rest.map(row)}
        </ul>
      )}
    </div>
  );
}
