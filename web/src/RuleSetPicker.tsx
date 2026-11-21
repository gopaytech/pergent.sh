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
      <ol className="files" aria-label="Rule sets in match order">
        {selected.map((n, i) => (
          <li key={n} data-rule-set={n} className="skill">
            <b className="grow">{n}</b>
            {!known.has(n) && <span className="problem">not in the library</span>}
            <button type="button" className="btn small" aria-label={`Move ${n} up`} disabled={disabled || i === 0} onClick={() => move(i, -1)}>
              ↑
            </button>
            <button
              type="button"
              className="btn small"
              aria-label={`Move ${n} down`}
              disabled={disabled || i === selected.length - 1}
              onClick={() => move(i, 1)}
            >
              ↓
            </button>
            <button type="button" className="btn small" aria-label={`Remove ${n}`} disabled={disabled} onClick={() => onChange(selected.filter((x) => x !== n))}>
              Remove
            </button>
          </li>
        ))}
      </ol>
      {selected.length === 0 && <div className="pad meta">No rule sets.</div>}
      <div className="pad">
        <select
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
        </select>
      </div>
    </div>
  );
}
