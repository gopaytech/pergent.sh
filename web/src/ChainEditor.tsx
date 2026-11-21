import { useState } from "react";

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
  }
  return e.state;
}

// ChainEditor shows a model chain, and edits it unless readOnly: the entries
// in the order reviews try them, with up, down and remove on each, and the
// choices to add at the end. The config file's entries (fixed) stay at the
// front and are never sent: onSave gets the other entries in order. An entry
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
  onSave,
  onClear,
}: {
  value: ChainView;
  choices: ChainEntryView[];
  readOnly: boolean;
  // allowEmpty lets an empty chain be saved (a team's: no chain of its own).
  allowEmpty?: boolean;
  busy?: boolean;
  onSave: (entries: ChainRef[]) => void;
  onClear: (entry: ChainEntryView) => void;
}) {
  const [draft, setDraft] = useState<ChainEntryView[]>(value.entries);
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
    setDraft([...draft, c]);
    setPick("");
  };

  const tooLong = draft.length > maxChain;
  const blocked = draft.some((e) => !kept(e));
  const empty = draft.length === 0;
  const canSave = !busy && !tooLong && !blocked && (allowEmpty || !empty);

  return (
    <div>
      <div role="list" aria-label="Model chain">
        {draft.map((e, i) => (
          <div className="link-row" role="listitem" key={refKey(e)}>
            <div className="link-head">
              <span className="meta">{i + 1}.</span>
              <span className="grow">
                <b>{e.name}</b>{" "}
                <span className="meta">
                  {e.provider} / {e.model} · {entryWhose(e)}
                </span>
              </span>
              {e.fixed && <span className="badge cfg">from the config file</span>}
              <span className={e.state === "ready" ? "meta" : "meta warn-text"}>{entryStateText(e)}</span>
              {e.canClear && e.class !== "" && (
                <button type="button" className="btn small" aria-label={`Clear mark of ${e.name}`} onClick={() => onClear(e)}>
                  Clear mark
                </button>
              )}
              {!readOnly && !e.fixed && (
                <span className="actions-row">
                  <button
                    type="button"
                    className="btn small"
                    aria-label={`Move ${e.name} up`}
                    disabled={i <= fixed}
                    onClick={() => move(i, -1)}
                  >
                    Up
                  </button>
                  <button
                    type="button"
                    className="btn small"
                    aria-label={`Move ${e.name} down`}
                    disabled={i === draft.length - 1}
                    onClick={() => move(i, 1)}
                  >
                    Down
                  </button>
                  <button
                    type="button"
                    className="btn small"
                    aria-label={`Remove ${e.name}`}
                    onClick={() => setDraft(draft.filter((_, k) => k !== i))}
                  >
                    Remove
                  </button>
                </span>
              )}
            </div>
            {!readOnly && !kept(e) && <div className="problem">You can't keep this entry here. Remove it to save.</div>}
          </div>
        ))}
      </div>
      {empty && <div className="pad meta">No entries.</div>}
      {!readOnly && (
        <div className="pad stack">
          <div className="actions-row">
            <select aria-label="Entry to add" value={pick} onChange={(ev) => setPick(ev.target.value)}>
              <option value="">{toAdd.length === 0 ? "Nothing more to add" : "Choose an entry to add"}</option>
              {toAdd.map((c) => (
                <option key={refKey(c)} value={refKey(c)}>
                  {c.name} ({entryWhose(c)})
                </option>
              ))}
            </select>
            <button type="button" className="btn small" disabled={pick === ""} onClick={add}>
              Add
            </button>
          </div>
          {tooLong && <div className="problem">A chain holds at most {maxChain} entries. Remove one to save.</div>}
          <div className="actions-row">
            <button
              type="button"
              className="btn primary small"
              disabled={!canSave}
              onClick={() => onSave(draft.filter((e) => !e.fixed).map((e) => ({ source: e.source, id: e.id })))}
            >
              Save chain
            </button>
          </div>
        </div>
      )}
    </div>
  );
}
