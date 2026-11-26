import { ArrowDown, ArrowUp } from "lucide-react";

import { Button } from "@/components/ui/button";
import { NativeSelect } from "@/components/ui/native-select";

import type { RuleSet } from "./api";

// RuleSetPicker chooses and orders rule sets of the library: the first entry
// that matches a file, across the sets in this order, gives it its rule. Only
// sets written in the dashboard are offered; a config-file set is named in
// the config file. A chosen name the library no longer has is marked, and
// saving it is refused.
export default function RuleSetPicker({
  label,
  library,
  selected,
  disabled,
  onChange,
}: {
  label: string;
  library: RuleSet[];
  selected: string[];
  disabled: boolean;
  onChange: (names: string[]) => void;
}) {
  const known = new Set(library.map((s) => s.name));
  const offered = library.filter((s) => s.source === "dashboard" && !selected.includes(s.name));
  const move = (i: number, by: number) => {
    const next = [...selected];
    [next[i], next[i + by]] = [next[i + by], next[i]];
    onChange(next);
  };
  return (
    <div role="group" aria-label={label}>
      <ol aria-label="Rule sets in match order">
        {selected.map((n, i) => (
          <li key={n} data-rule-set={n} className="flex items-center gap-2 border-b px-4 py-2.5 text-sm">
            <b className="min-w-0 grow font-semibold">{n}</b>
            {!known.has(n) && <span className="text-bad text-sm">not in the library</span>}
            <Button type="button" variant="outline" size="icon-sm" aria-label={`Move ${n} up`} disabled={disabled || i === 0} onClick={() => move(i, -1)}>
              <ArrowUp aria-hidden="true" />
            </Button>
            <Button
              type="button"
              variant="outline"
              size="icon-sm"
              aria-label={`Move ${n} down`}
              disabled={disabled || i === selected.length - 1}
              onClick={() => move(i, 1)}
            >
              <ArrowDown aria-hidden="true" />
            </Button>
            <Button
              type="button"
              variant="outline"
              size="sm"
              aria-label={`Remove ${n}`}
              disabled={disabled}
              onClick={() => onChange(selected.filter((x) => x !== n))}
            >
              Remove
            </Button>
          </li>
        ))}
      </ol>
      {selected.length === 0 && <div className="text-muted-foreground border-b px-4 py-2.5 text-sm">No rule sets.</div>}
      <div className="border-b px-4 py-2.5">
        <NativeSelect
          className="max-w-xs"
          aria-label="Add a rule set"
          value=""
          disabled={disabled || offered.length === 0}
          onChange={(e) => e.target.value && onChange([...selected, e.target.value])}
        >
          <option value="">Add a rule set…</option>
          {offered.map((s) => (
            <option key={s.name} value={s.name}>
              {s.name}
            </option>
          ))}
        </NativeSelect>
      </div>
    </div>
  );
}
