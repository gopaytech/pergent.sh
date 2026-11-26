import { useState } from "react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { NativeSelect } from "@/components/ui/native-select";
import { cn } from "@/lib/utils";

import type { ChainEntryView, ChainRef, ChainView } from "./api";
import { utcText } from "./ui";

// The most entries a chain holds, the config file's included; the server
// refuses a longer one.
export const maxChain = 5;

const refKey = (e: { source: string; id: string }) => `${e.source}:${e.id}`;

// entryWhose says whose an entry is: global, or the team's.
export const entryWhose = (e: ChainEntryView) => (e.source === "team" ? `team ${e.team}` : "global");

// entryStateText says in words what reviews do with an entry now.
export function entryStateText(e: ChainEntryView): string {
  switch (e.state) {
    case "ready":
      return "ready";
    case "exhausted":
      return e.until ? `exhausted until ${utcText(e.until)} (${e.class})` : `exhausted (${e.class})`;
    case "key_rejected":
      return "key rejected (auth)";
    case "removed":
      return "removed";
    case "not_allowed":
      return "not allowed for teams any more";
    case "not_owner":
      return "team no longer owns this repository";
    case "key_unusable":
      return "key can't be read";
    case "model_limit":
      return "model limit used up";
    case "team_limit":
      return "team limit used up";
  }
  return e.state;
}

// ChainEditor shows a model chain, and edits it unless readOnly: the entries
// in the order reviews try them, with up, down and remove on each, and the
// choices to add at the end. In the global chain the config file's entries
// (fixed) stay at the front and are never sent: onSave gets the other
// entries in order. In any other chain they are entries like the rest. An entry
// that is not one of choices can't be saved (the server would refuse the
// whole chain), so it is flagged and Save waits until it is removed. Clear
// mark shows on an entry whose mark the session may clear (canClear) and
// calls onClear with it. A new value from the server should remount the
// editor (a key), which starts it again from that value.
export default function ChainEditor({
  value,
  choices,
  readOnly,
  allowEmpty,
  busy,
  label = "Model chain",
  note,
  onCancel,
  onSave,
  onClear,
}: {
  value: ChainView;
  choices: ChainEntryView[];
  readOnly: boolean;
  // allowEmpty lets an empty chain be saved (a team's: no chain of its own).
  allowEmpty?: boolean;
  busy?: boolean;
  // label names the list: each chain of the Reviewers card has its own.
  label?: string;
  // note is the caller's word on the last save, shown beside Save chain.
  note?: string;
  // onCancel, when given, puts a Cancel beside Save chain: for an editor
  // that was opened by a button, so it can be closed again.
  onCancel?: () => void;
  onSave: (entries: ChainRef[]) => void;
  onClear: (entry: ChainEntryView) => void;
}) {
  // The server marks the config file's entries fixed in every chain; they
  // stay in front of the global chain only.
  const [draft, setDraft] = useState<ChainEntryView[]>(() =>
    value.entries.map((e) => (e.fixed && value.scope !== "global" ? { ...e, fixed: false } : e)),
  );
  const [pick, setPick] = useState("");
  const allowed = new Set(choices.map(refKey));
  const fixed = draft.filter((e) => e.fixed).length;
  const kept = (e: ChainEntryView) => e.fixed || allowed.has(refKey(e));
  const inDraft = new Set(draft.map(refKey));
  const toAdd = choices.filter((c) => !inDraft.has(refKey(c)));

  const move = (i: number, by: number) => {
    const next = [...draft];
    [next[i], next[i + by]] = [next[i + by], next[i]];
    setDraft(next);
  };
  const add = () => {
    const c = toAdd.find((x) => refKey(x) === pick);
    if (!c) return;
    setDraft([...draft, { ...c, fixed: c.fixed && value.scope === "global" }]);
    setPick("");
  };

  const tooLong = draft.length > maxChain;
  const blocked = draft.some((e) => !kept(e));
  const empty = draft.length === 0;
  const canSave = !busy && !tooLong && !blocked && (allowEmpty || !empty);

  return (
    <div>
      <div role="list" aria-label={label}>
        {draft.map((e, i) => (
          <div className="grid gap-2 border-t px-4 py-3" role="listitem" key={refKey(e)}>
            <div className="flex flex-wrap items-center gap-3 text-sm">
              <span className="text-muted-foreground tabular-nums">{i + 1}.</span>
              <span className="min-w-0 grow">
                <b className="font-semibold">{e.name}</b>{" "}
                <span className="text-muted-foreground">
                  {e.provider} / {e.model} · {entryWhose(e)}
                </span>
              </span>
              {e.fixed && (
                <Badge variant="grey" className="badge cfg">
                  from the config file
                </Badge>
              )}
              <span className={cn("text-xs", e.state === "ready" ? "text-muted-foreground" : "warn-text text-warn")}>{entryStateText(e)}</span>
              {e.canClear && e.class !== "" && (
                <Button type="button" variant="outline" size="sm" aria-label={`Clear mark of ${e.name}`} onClick={() => onClear(e)}>
                  Clear mark
                </Button>
              )}
              {!readOnly && !e.fixed && (
                <span className="flex gap-2">
                  <Button
                    type="button"
                    variant="outline"
                    size="sm"
                    aria-label={`Move ${e.name} up`}
                    disabled={i <= fixed}
                    onClick={() => move(i, -1)}
                  >
                    Up
                  </Button>
                  <Button
                    type="button"
                    variant="outline"
                    size="sm"
                    aria-label={`Move ${e.name} down`}
                    disabled={i === draft.length - 1}
                    onClick={() => move(i, 1)}
                  >
                    Down
                  </Button>
                  <Button
                    type="button"
                    variant="outline"
                    size="sm"
                    aria-label={`Remove ${e.name}`}
                    onClick={() => setDraft(draft.filter((_, k) => k !== i))}
                  >
                    Remove
                  </Button>
                </span>
              )}
            </div>
            {!readOnly && !kept(e) && <div className="text-bad text-sm">You can't keep this entry here. Remove it to save.</div>}
          </div>
        ))}
      </div>
      {empty && <div className="text-muted-foreground border-t px-4 py-2.5 text-sm">No entries.</div>}
      {!readOnly && (
        <div className="grid gap-2 border-t px-4 py-3">
          <div className="flex items-center gap-2">
            <NativeSelect className="max-w-sm" aria-label="Entry to add" value={pick} onChange={(ev) => setPick(ev.target.value)}>
              <option value="">{toAdd.length === 0 ? "Nothing more to add" : "Choose an entry to add"}</option>
              {toAdd.map((c) => (
                <option key={refKey(c)} value={refKey(c)}>
                  {c.name} ({entryWhose(c)})
                </option>
              ))}
            </NativeSelect>
            <Button type="button" variant="outline" size="sm" disabled={pick === ""} onClick={add}>
              Add
            </Button>
          </div>
          {tooLong && <div className="text-bad text-sm">A chain holds at most {maxChain} entries. Remove one to save.</div>}
        </div>
      )}
      {!readOnly && (
        <div className="bg-muted/40 flex items-center justify-end gap-2 border-t px-4 py-3">
          {note && (
            <span className="text-muted-foreground grow text-sm" role="status">
              {note}
            </span>
          )}
          {onCancel && (
            <Button type="button" variant="outline" size="sm" disabled={busy} onClick={onCancel}>
              Cancel
            </Button>
          )}
          <Button
            type="button"
            size="sm"
            disabled={!canSave}
            onClick={() => onSave(draft.filter((e) => !e.fixed).map((e) => ({ source: e.source, id: e.id })))}
          >
            Save chain
          </Button>
        </div>
      )}
    </div>
  );
}
