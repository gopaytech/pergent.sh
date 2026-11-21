import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";

import { api, paths, type ChainEntryView, type ChainRef, type ChainView, type MyTeamView } from "../api";
import ChainEditor from "../ChainEditor";
import { useWrite } from "../layout/write";
import { ErrorBox, Loading } from "../ui";

// inheritText says where an inherited chain comes from.
const inheritText = (c: ChainView) =>
  c.source === "team" ? `Inherited from the chain of its owner team, ${c.team}.` : "Inherited from the global chain.";

// RepoModelChain is a repository's model chain: the entries its reviews try
// in order. It inherits its owner team's chain, else the global one, until it
// is given its own. Everyone who sees the repository sees it; an admin and a
// maintainer of its owner team change it (the server says which: readOnly).
export default function RepoModelChain({ instance, project }: { instance: string; project: string }) {
  const client = useQueryClient();
  const write = useWrite();
  const key = ["repo-chain", instance, project];
  const chain = useQuery({ queryKey: key, queryFn: () => api.get<ChainView>(paths.apiRepoChain(instance, project)) });
  const [starting, setStarting] = useState(false);
  const [note, setNote] = useState("");
  const done = (next: ChainView, message: string) => {
    client.setQueryData(key, next);
    // The repository page names position 1 of the chain in effect.
    client.invalidateQueries({ queryKey: ["repo", instance] });
    setStarting(false);
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
  // A global entry's mark is cleared on the admins' route; a team entry's on
  // its team's, found by the team's name among the session's teams.
  const clear = useMutation({
    mutationFn: async (e: ChainEntryView) => {
      if (e.source === "global") return write<void>("DELETE", paths.apiAdminModelMark(e.id));
      const teams = await client.fetchQuery({ queryKey: ["my-teams"], queryFn: () => api.get<MyTeamView[]>("/api/me/teams") });
      const team = teams.find((t) => t.name === e.team);
      if (!team) throw new Error(`The team ${e.team} is not one of yours.`);
      return write<void>("DELETE", paths.apiTeamModelMark(team.id, e.id));
    },
    onSuccess: () => client.invalidateQueries({ queryKey: key }),
  });
  const busy = save.isPending || reset.isPending;
  return (
    <section className="card" aria-label="Model chain">
      <h2>Model chain</h2>
      <div className="pad meta">
        The models this repository's reviews try, in order. When the provider says an entry is out of quota or credit, or
        rejects its key, the review starts again on the next one.
      </div>
      {chain.isPending && <Loading />}
      {chain.isError && <ErrorBox error={chain.error} />}
      {chain.isSuccess && (
        <ChainBody
          chain={chain.data}
          starting={starting}
          busy={busy}
          onStart={() => {
            setNote("");
            setStarting(true);
          }}
          onSave={(entries) => save.mutate(entries)}
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
    </section>
  );
}

function ChainBody({
  chain,
  starting,
  busy,
  onStart,
  onSave,
  onReset,
  onClear,
}: {
  chain: ChainView;
  starting: boolean;
  busy: boolean;
  onStart: () => void;
  onSave: (entries: ChainRef[]) => void;
  onReset: () => void;
  onClear: (e: ChainEntryView) => void;
}) {
  const own = chain.source === "repository";
  // An inherited chain is shown as it is; the repository's own chain starts
  // from it, and the server's choices say what it may hold.
  const editing = !chain.readOnly && (own || starting);
  return (
    <>
      <div className="pad meta">{own ? "This repository has its own chain." : inheritText(chain)}</div>
      {own && chain.savedByAdmin && <div className="pad meta">An admin saved this chain.</div>}
      <ChainEditor
        // A new answer from the server, or the start of an own chain, starts the editor again.
        key={`${JSON.stringify(chain)}:${editing}`}
        value={chain}
        choices={editing ? chain.choices : []}
        readOnly={!editing}
        busy={busy}
        onSave={onSave}
        onClear={onClear}
      />
      {chain.readOnly && <div className="pad meta">Only admins and maintainers of the repository's owner team change its chain.</div>}
      {!chain.readOnly && !own && !starting && (
        <div className="pad">
          <button type="button" className="btn small" disabled={busy} onClick={onStart}>
            Give it its own chain
          </button>
        </div>
      )}
      {!chain.readOnly && own && (
        <div className="pad">
          <button type="button" className="btn small" disabled={busy} onClick={onReset}>
            Inherit again
          </button>
        </div>
      )}
    </>
  );
}
