import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";

import { paths, type ChainEntryView, type ChainRef, type ChainView } from "../api";
import ChainEditor from "../ChainEditor";
import { useWrite } from "../layout/write";
import ReviewersCard, { repoChainQuery, useClearMark } from "../ReviewersCard";
import { ErrorBox, Loading } from "../ui";

// inheritText says where an inherited chain comes from.
const inheritText = (c: ChainView) =>
  c.source === "team" ? `Inherited from the chain of its owner team, ${c.team}.` : "Inherited from the global chain.";

// RepoModelChain is a repository's Reviewers card. Reviewer 1 is its model
// chain: the entries its reviews try in order. It inherits its owner team's
// chain, else the global one, until it is given its own. Reviewers 2 to 5
// and the judge are the card's (ReviewersCard). Everyone who sees the
// repository sees it; an admin and a maintainer of its owner team change it
// (the server says which: readOnly).
export default function RepoModelChain({ instance, project }: { instance: string; project: string }) {
  return <ReviewersCard instance={instance} project={project} first={<ReviewerOne instance={instance} project={project} />} />;
}

// ReviewerOne is reviewer 1: the model chain in effect, with Edit. Editing
// an inherited chain gives the repository its own, starting from it; Inherit
// again drops the repository's own chain.
function ReviewerOne({ instance, project }: { instance: string; project: string }) {
  const client = useQueryClient();
  const write = useWrite();
  const query = repoChainQuery(instance, project);
  const chain = useQuery(query);
  const [editing, setEditing] = useState(false);
  const [note, setNote] = useState("");
  const done = (next: ChainView, message: string) => {
    client.setQueryData(query.queryKey, next);
    // The repository page names position 1 of the chain in effect, and the
    // reviewers' warnings compare their first models with it.
    client.invalidateQueries({ queryKey: ["repo", instance] });
    client.invalidateQueries({ queryKey: ["repo-reviewers", instance, project] });
    setEditing(false);
    setNote(message);
  };
  const save = useMutation({
    mutationFn: (entries: ChainRef[]) => write<ChainView>("PUT", paths.apiRepoChain(instance, project), { entries }),
    onMutate: () => setNote(""),
    onSuccess: (next) => done(next, "Saved."),
  });
  const reset = useMutation({
    mutationFn: () => write<ChainView>("POST", paths.apiRepoChainReset(instance, project), {}),
    onMutate: () => setNote(""),
    onSuccess: (next) => done(next, "It inherits its chain again."),
  });
  const clear = useClearMark();
  const busy = save.isPending || reset.isPending;
  return (
    <div role="group" aria-label="Reviewer 1">
      <div className="pad actions-row">
        <b className="grow">Reviewer 1</b>
        {chain.isSuccess && !chain.data.readOnly && !editing && (
          <button
            type="button"
            className="btn small"
            aria-label="Edit reviewer 1"
            disabled={busy}
            onClick={() => {
              setNote("");
              setEditing(true);
            }}
          >
            Edit
          </button>
        )}
      </div>
      {chain.isPending && <Loading />}
      {chain.isError && <ErrorBox error={chain.error} />}
      {chain.isSuccess && (
        <ChainBody
          chain={chain.data}
          editing={editing}
          busy={busy}
          onSave={(entries) => save.mutate(entries)}
          onCancel={() => setEditing(false)}
          onReset={() => {
            if (window.confirm("Drop this repository's own chain? Its reviews then use its owner team's chain, or the global chain.")) {
              reset.mutate();
            }
          }}
          onClear={(e) => clear.mutate(e)}
        />
      )}
      {note && (
        <div className="pad note" role="status">
          {note}
        </div>
      )}
      {save.isError && <ErrorBox error={save.error} />}
      {reset.isError && <ErrorBox error={reset.error} />}
      {clear.isError && <ErrorBox error={clear.error} />}
    </div>
  );
}

function ChainBody({
  chain,
  editing: asked,
  busy,
  onSave,
  onCancel,
  onReset,
  onClear,
}: {
  chain: ChainView;
  editing: boolean;
  busy: boolean;
  onSave: (entries: ChainRef[]) => void;
  onCancel: () => void;
  onReset: () => void;
  onClear: (e: ChainEntryView) => void;
}) {
  const own = chain.source === "repository";
  // The editor starts from the chain in effect, and the server's choices say
  // what it may hold.
  const editing = !chain.readOnly && asked;
  return (
    <>
      <div className="pad meta">{own ? "This repository has its own chain." : inheritText(chain)}</div>
      {own && chain.savedByAdmin && <div className="pad meta">An admin saved this chain.</div>}
      <ChainEditor
        // A new answer from the server, or the start of an edit, starts the editor again.
        key={`${JSON.stringify(chain)}:${editing}`}
        label="Reviewer 1 chain"
        value={chain}
        choices={editing ? chain.choices : []}
        readOnly={!editing}
        busy={busy}
        onSave={onSave}
        onClear={onClear}
      />
      {editing && (
        <div className="pad actions-row">
          <button type="button" className="btn small" disabled={busy} onClick={onCancel}>
            Cancel
          </button>
          {own && (
            <button type="button" className="btn small" disabled={busy} onClick={onReset}>
              Inherit again
            </button>
          )}
        </div>
      )}
    </>
  );
}
