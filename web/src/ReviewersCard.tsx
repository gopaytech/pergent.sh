import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState, type ReactNode } from "react";

import { api, paths, type ChainEntryView, type ChainRef, type ChainView, type MyTeamView, type ReviewersView } from "./api";
import ChainEditor from "./ChainEditor";
import { FoldCard, countText } from "./Fold";
import { useWrite } from "./layout/write";
import { addReviewer, costHint, draftOf, removeReviewer, setReviewer, type ReviewersDraft } from "./reviewers";
import { ErrorBox, Loading } from "./ui";

// repoChainQuery reads reviewer 1, the repository's model chain in effect.
export const repoChainQuery = (instance: string, project: string) => ({
  queryKey: ["repo-chain", instance, project],
  queryFn: () => api.get<ChainView>(paths.apiRepoChain(instance, project)),
});

// useClearMark clears a chain entry's mark. A global entry's is cleared on the
// admins' route; a team entry's on its team's, found by the team's name among
// the session's teams. It then reads every chain of the card again.
export function useClearMark() {
  const client = useQueryClient();
  const write = useWrite();
  return useMutation({
    mutationFn: async (e: ChainEntryView) => {
      if (e.source === "global") return write<void>("DELETE", paths.apiAdminModelMark(e.id));
      const teams = await client.fetchQuery({ queryKey: ["my-teams"], queryFn: () => api.get<MyTeamView[]>("/api/me/teams") });
      const team = teams.find((t) => t.name === e.team);
      if (!team) throw new Error(`The team ${e.team} is not one of yours.`);
      return write<void>("DELETE", paths.apiTeamModelMark(team.id, e.id));
    },
    onSuccess: () => {
      // Reviewer 1, the other reviewers and the judge may show the same mark.
      client.invalidateQueries({ queryKey: ["repo-chain"] });
      client.invalidateQueries({ queryKey: ["repo-reviewers"] });
    },
  });
}

// open is the one chain of the card being edited: reviewer n (2..5), the
// new reviewer, or the judge.
type Open = number | "new" | "judge" | null;

// ReviewersCard is a repository's reviewers: reviewer 1 (first, the model
// chain in effect, edited on its own route) and reviewers 2 to 5 with their
// judge, saved together in one PUT. With 2 or more reviewers, they review at
// the same time and the judge checks their findings. Anyone who sees the
// repository sees the card; admins and maintainers of its owner team change
// it (the server says which: readOnly). A refused save shows the server's
// message.
export default function ReviewersCard({
  instance,
  project,
  first,
  defaultOpen = true,
}: {
  instance: string;
  project: string;
  first: ReactNode;
  // false starts the card folded, as on a repository's Settings tab.
  defaultOpen?: boolean;
}) {
  const client = useQueryClient();
  const write = useWrite();
  const key = ["repo-reviewers", instance, project];
  const view = useQuery({ queryKey: key, queryFn: () => api.get<ReviewersView>(paths.apiRepoReviewers(instance, project)) });
  const one = useQuery(repoChainQuery(instance, project));
  const [open, setOpen] = useState<Open>(null);
  const [note, setNote] = useState("");
  const save = useMutation({
    mutationFn: (d: ReviewersDraft) => write<ReviewersView>("PUT", paths.apiRepoReviewers(instance, project), d),
    onMutate: () => setNote(""),
    onSuccess: (next) => {
      client.setQueryData(key, next);
      // The repository page has the multi review line.
      client.invalidateQueries({ queryKey: ["repo", instance] });
      setOpen(null);
      setNote("Saved.");
    },
  });
  const clear = useClearMark();
  const v = view.data;
  const busy = save.isPending;
  const draft = v ? draftOf(v) : null;
  const send = (d: ReviewersDraft) => save.mutate(d);
  // edit starts editing one chain, and drops what the last save said.
  const edit = (o: Open) => {
    save.reset();
    setNote("");
    setOpen(o);
  };
  // empty is a chain with no entries, to start a new reviewer or a judge
  // from: reviewer 1's shape and choices.
  const empty = (c: ChainView | null): ChainView | null =>
    c ? { ...c, source: "repository", team: "", savedByAdmin: false, entries: [] } : null;
  const reviewers = 1 + (v?.reviewers.length ?? 0);
  // added is the number a new reviewer gets.
  const added = reviewers + 1;
  const start = empty(one.data ?? null);
  // judge is the judge chain, or an empty one while there is no judge row.
  const judge = v?.judge ?? start;

  return (
    <FoldCard
      label="Reviewers"
      title="Reviewers"
      summary={reviewers === 1 ? "1 reviewer, multi review off" : `${countText(reviewers, "reviewer")} and a judge`}
      defaultOpen={defaultOpen}
    >
      <div className="pad meta">
        Each reviewer is a model chain: the models a review tries, in order. When the provider says an entry is out of quota or
        credit, or rejects its key, the review starts again on the next one. With 2 or more reviewers, they all review each change
        at the same time, and a judge checks their findings and writes one result. Multi review is experimental.
      </div>
      {v?.warnings.map((w) => (
        <div className="alert" role="note" key={w}>
          {w}
        </div>
      ))}
      {first}
      {view.isPending && <Loading />}
      {view.isError && <ErrorBox error={view.error} />}
      {v && draft && (
        <>
          {v.reviewers.map((r) => (
            <ChainGroup
              key={`${r.number}:${JSON.stringify(r.chain)}`}
              name={`Reviewer ${r.number}`}
              title={
                <>
                  <b>Reviewer {r.number}</b> <span className="meta">(multi review, experimental)</span>
                </>
              }
              chain={r.chain}
              readOnly={v.readOnly}
              editing={open === r.number}
              busy={busy}
              editLabel={`Edit reviewer ${r.number}`}
              onEdit={() => edit(r.number)}
              onCancel={() => setOpen(null)}
              onRemove={() => {
                // It saves at once, unlike the chain edits: ask first, and say
                // when it turns multi review off.
                const last = v.reviewers.length === 1;
                const question = `Remove reviewer ${r.number}? It saves now.${last ? " Multi review turns off, and the judge is no longer used." : ""}`;
                if (!window.confirm(question)) return;
                setOpen(null);
                send(removeReviewer(draft, r.number));
              }}
              onSave={(entries) => send(setReviewer(draft, r.number, entries))}
              onClear={(e) => clear.mutate(e)}
            />
          ))}
          {open === "new" && start && (
            <ChainGroup
              name={`Reviewer ${added}`}
              title={
                <>
                  <b>Reviewer {added}</b> <span className="meta">(multi review, experimental)</span>
                </>
              }
              chain={start}
              readOnly={false}
              editing
              busy={busy}
              editLabel=""
              onEdit={() => {}}
              onCancel={() => setOpen(null)}
              onSave={(entries) => send(addReviewer(draft, entries))}
              onClear={(e) => clear.mutate(e)}
            />
          )}
          {!v.readOnly && open !== "new" && (
            <div className="pad">
              <button
                type="button"
                className="btn small"
                disabled={busy || v.addRefusal !== "" || !start}
                title={v.addRefusal || undefined}
                onClick={() => edit("new")}
              >
                Add a reviewer
              </button>
            </div>
          )}
          {v.reviewers.length > 0 && judge && (
            <ChainGroup
              key={`judge:${JSON.stringify(v.judge)}`}
              name="Judge"
              title={<b>Judge</b>}
              intro="The judge checks the findings of every reviewer against the code, and writes one result."
              chain={judge}
              readOnly={v.readOnly}
              editing={open === "judge"}
              busy={busy}
              editLabel="Edit the judge"
              onEdit={() => edit("judge")}
              onCancel={() => setOpen(null)}
              onSave={(entries) => send({ ...draft, judge: entries })}
              onClear={(e) => clear.mutate(e)}
            />
          )}
          {v.readOnly && <div className="pad meta">Only admins and maintainers of the repository's owner team change its reviewers.</div>}
          {costHint(reviewers) && <div className="pad meta">{costHint(reviewers)}</div>}
        </>
      )}
      {note && (
        <div className="pad note" role="status">
          {note}
        </div>
      )}
      {save.isError && <ErrorBox error={save.error} />}
      {clear.isError && <ErrorBox error={clear.error} />}
    </FoldCard>
  );
}

// ChainGroup is one chain of the card after reviewer 1: its title, Edit and
// (for a reviewer) Remove, and the chain, which ChainEditor edits while
// editing. A new answer from the server remounts it (its key).
function ChainGroup({
  name,
  title,
  intro,
  chain,
  readOnly,
  editing,
  busy,
  editLabel,
  onEdit,
  onCancel,
  onRemove,
  onSave,
  onClear,
}: {
  name: string;
  title: ReactNode;
  intro?: string;
  chain: ChainView;
  readOnly: boolean;
  editing: boolean;
  busy: boolean;
  editLabel: string;
  onEdit: () => void;
  onCancel: () => void;
  onRemove?: () => void;
  onSave: (entries: ChainRef[]) => void;
  onClear: (e: ChainEntryView) => void;
}) {
  const canEdit = !readOnly && editing;
  return (
    <div role="group" aria-label={name}>
      <div className="pad actions-row">
        <span className="grow">{title}</span>
        {!readOnly && !editing && (
          <button type="button" className="btn small" aria-label={editLabel} disabled={busy} onClick={onEdit}>
            Edit
          </button>
        )}
        {!readOnly && onRemove && (
          <button type="button" className="btn small danger" aria-label={`Remove ${name.toLowerCase()}`} disabled={busy} onClick={onRemove}>
            Remove
          </button>
        )}
      </div>
      {intro && <div className="pad meta">{intro}</div>}
      {chain.savedByAdmin && <div className="pad meta">An admin saved this chain.</div>}
      <ChainEditor
        // The start of an edit starts the editor again from the chain.
        key={String(canEdit)}
        label={`${name} chain`}
        value={chain}
        choices={canEdit ? chain.choices : []}
        readOnly={!canEdit}
        busy={busy}
        onSave={onSave}
        onClear={onClear}
      />
      {canEdit && (
        <div className="pad">
          <button type="button" className="btn small" disabled={busy} onClick={onCancel}>
            Cancel
          </button>
        </div>
      )}
    </div>
  );
}
