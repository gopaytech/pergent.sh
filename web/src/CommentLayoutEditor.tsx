import { useState } from "react";

import { ArrowDown, ArrowUp } from "lucide-react";

import { Button } from "@/components/ui/button";
import { NativeSelect } from "@/components/ui/native-select";

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

// rowClass is one block's row: show box, name, state and the move buttons.
const rowClass = "flex flex-wrap items-center gap-2.5 border-b px-4 py-2.5 text-sm";

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
    return <div className="text-muted-foreground px-4 py-2.5 text-sm">Will follow the default layout once you save. Reload to undo.</div>;
  }
  const reset = (
    <Button type="button" variant="outline" size="sm" disabled={disabled || value === null} onClick={() => onChange(null)}>
      Reset to default
    </Button>
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
          <div className={rowClass} role="listitem" key={e.block}>
            {e.block === "findings" ? (
              <span className="size-4 shrink-0" aria-hidden="true" />
            ) : (
              <input
                type="checkbox"
                className="accent-primary size-4 shrink-0"
                aria-label={`Show ${label(e.block)}`}
                checked
                disabled={disabled}
                onChange={() => hide(i)}
              />
            )}
            <b className="min-w-0 grow font-semibold">{label(e.block)}</b>
            <NativeSelect
              className="w-36"
              aria-label={`${label(e.block)} state`}
              value={e.state}
              disabled={disabled}
              onChange={(ev) => emit(entries.map((x, j) => (j === i ? { ...x, state: ev.target.value } : x)))}
            >
              <option value="open">Open</option>
              <option value="collapsed">Collapsed</option>
            </NativeSelect>
            <Button
              type="button"
              variant="outline"
              size="icon-sm"
              aria-label={`Move ${label(e.block)} up`}
              disabled={disabled || i === 0}
              onClick={() => move(i, -1)}
            >
              <ArrowUp aria-hidden="true" />
            </Button>
            <Button
              type="button"
              variant="outline"
              size="icon-sm"
              aria-label={`Move ${label(e.block)} down`}
              disabled={disabled || i === entries.length - 1}
              onClick={() => move(i, 1)}
            >
              <ArrowDown aria-hidden="true" />
            </Button>
            {e.block === "findings" && (
              <span className="text-muted-foreground basis-full pl-6.5 text-xs">
                Blocking findings always stay open; collapsed applies to suggestions.
              </span>
            )}
          </div>
        ))}
        {left.map((b) => (
          <div className={rowClass} role="listitem" key={b}>
            <input
              type="checkbox"
              className="accent-primary size-4 shrink-0"
              aria-label={`Show ${label(b)}`}
              checked={false}
              disabled={disabled}
              onChange={() => emit([...entries, { block: b, state: lastState(b) }])}
            />
            <b className="min-w-0 grow font-semibold">{label(b)}</b>
            <NativeSelect className="w-36" aria-label={`${label(b)} state`} value={lastState(b)} disabled>
              <option value="open">Open</option>
              <option value="collapsed">Collapsed</option>
            </NativeSelect>
          </div>
        ))}
      </div>
      <div className="text-muted-foreground flex flex-wrap items-center gap-2 px-4 py-2.5 text-sm">
        A block that is not shown is left out of the comment. {reset}
      </div>
    </div>
  );
}
