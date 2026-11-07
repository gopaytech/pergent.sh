import { useState } from "react";

// The labels of the comment's blocks. The server sends the layout, so the
// default order and states live there; this only names the blocks, and holds
// the list a left-out block is picked from. A block it doesn't know is shown
// by its name.
const labels: Record<string, string> = {
  summary: "Summary",
  findings: "Findings",
  dismissed: "Dismissed",
  checks: "Checks",
  files: "Files",
};

type Entry = { block: string; state: string };

const parse = (list: string[]): Entry[] =>
  list.map((e) => {
    const i = e.indexOf(":");
    return { block: e.slice(0, i), state: e.slice(i + 1) };
  });

const text = (e: Entry) => `${e.block}:${e.state}`;

// CommentLayoutEditor edits the layout of a review comment: one row per block,
// in the order of value, each with a show box, a state and the buttons that
// move it. A block left out of value is an unticked row at the end, with the
// state it last had. value null is the default layout: defaults, when the page
// has it, is what that looks like. Every change calls onChange with the whole
// list; Reset calls it with null.
export default function CommentLayoutEditor({
  value,
  onChange,
  defaults,
  disabled,
}: {
  value: string[] | null;
  onChange: (value: string[] | null) => void;
  defaults?: string[];
  disabled?: boolean;
}) {
  // The state of each block the user hid here, so showing it again gives it back.
  const [hidden, setHidden] = useState<Record<string, string>>({});
  const list = value ?? defaults ?? null;
  if (list === null) {
    return <div className="pad meta">This repository follows the default layout.</div>;
  }
  const reset = (
    <button type="button" className="btn small" disabled={disabled || value === null} onClick={() => onChange(null)}>
      Reset to default
    </button>
  );
  const entries = parse(list);
  const left = Object.keys(labels).filter((b) => !entries.some((e) => e.block === b));
  const label = (b: string) => labels[b] ?? b;
  const emit = (next: Entry[]) => onChange(next.map(text));
  const move = (i: number, by: number) => {
    const next = [...entries];
    [next[i], next[i + by]] = [next[i + by], next[i]];
    emit(next);
  };
  const hide = (i: number) => {
    setHidden((h) => ({ ...h, [entries[i].block]: entries[i].state }));
    emit(entries.filter((_, j) => j !== i));
  };
  // A block that was never in the list takes the state the default gives it.
  const lastState = (b: string) => hidden[b] ?? parse(defaults ?? []).find((e) => e.block === b)?.state ?? "open";
  return (
    <div>
      <div role="list" aria-label="Comment layout">
        {entries.map((e, i) => (
          <div className="skill" role="listitem" key={e.block}>
            {e.block === "findings" ? (
              <span aria-hidden="true" />
            ) : (
              <input
                type="checkbox"
                aria-label={`Show ${label(e.block)}`}
                checked
                disabled={disabled}
                onChange={() => hide(i)}
              />
            )}
            <b className="grow">{label(e.block)}</b>
            {e.block === "findings" && (
              <span className="meta">Blocking findings always stay open; collapsed applies to suggestions.</span>
            )}
            <select
              aria-label={`${label(e.block)} state`}
              value={e.state}
              disabled={disabled}
              onChange={(ev) => emit(entries.map((x, j) => (j === i ? { ...x, state: ev.target.value } : x)))}
            >
              <option value="open">Open</option>
              <option value="collapsed">Collapsed</option>
            </select>
            <button
              type="button"
              className="btn small"
              aria-label={`Move ${label(e.block)} up`}
              disabled={disabled || i === 0}
              onClick={() => move(i, -1)}
            >
              ↑
            </button>
            <button
              type="button"
              className="btn small"
              aria-label={`Move ${label(e.block)} down`}
              disabled={disabled || i === entries.length - 1}
              onClick={() => move(i, 1)}
            >
              ↓
            </button>
          </div>
        ))}
        {left.map((b) => (
          <div className="skill" role="listitem" key={b}>
            <input
              type="checkbox"
              aria-label={`Show ${label(b)}`}
              checked={false}
              disabled={disabled}
              onChange={() => emit([...entries, { block: b, state: lastState(b) }])}
            />
            <b className="grow">{label(b)}</b>
            <select aria-label={`${label(b)} state`} value={lastState(b)} disabled>
              <option value="open">Open</option>
              <option value="collapsed">Collapsed</option>
            </select>
          </div>
        ))}
      </div>
      <div className="pad meta">A block that is not shown is left out of the comment. {reset}</div>
    </div>
  );
}
