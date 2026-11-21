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
  return (
    <>
      <RepoModelChainA instance={instance} project={project} />
      <RepoPanelChain instance={instance} project={project} />
    </>
  );
}

// useClearMark clears a chain entry's mark. A global entry's is cleared on the
// admins' route; a team entry's on its team's, found by the team's name among
// the session's teams. It then reads the chain at key again.
function useClearMark(key: string[]) {
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
    onSuccess: () => client.invalidateQueries({ queryKey: key }),
  });
}

function RepoModelChainA({ instance, project }: { instance: string; project: string }) {
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
  const clear = useClearMark(key);
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

// modeText says in words what the repository's chain B mode means.
const modeText = (c: ChainView) => {
  if (c.mode === "off") return "Panel: off for this repository";
  if (c.mode === "custom") return "This repository has its own chain B.";
  if (c.entries.length === 0) return "This repository has no chain B, so no review panel.";
  return c.source === "team"
    ? `Inherited from the chain B of its owner team, ${c.team}.`
    : "Inherited from the global chain B.";
};

// RepoPanelChain is a repository's chain B, the review panel's second chain,
// with a mode: Inherit (its owner team's chain B, else the global one),
// Custom (its own) or Off (no panel). Everyone who sees the repository sees
// it; who may change it is chain A's rule (the server says: readOnly). A save
// the image check refuses shows its message.
function RepoPanelChain({ instance, project }: { instance: string; project: string }) {
  const client = useQueryClient();
  const write = useWrite();
  const key = ["repo-panel-chain", instance, project];
  const chain = useQuery({ queryKey: key, queryFn: () => api.get<ChainView>(paths.apiRepoPanelChain(instance, project)) });
  const [starting, setStarting] = useState(false);
  const [note, setNote] = useState("");
  const done = (next: ChainView, message: string) => {
    client.setQueryData(key, next);
    // The repository page has the panel line.
    client.invalidateQueries({ queryKey: ["repo", instance] });
    setStarting(false);
    setNote(message);
  };
  const save = useMutation({
    mutationFn: (entries: ChainRef[]) => write<ChainView>("PUT", paths.apiRepoPanelChain(instance, project), { entries }),
    onMutate: () => setNote(""),
    onSuccess: (next) => done(next, "Saved."),
  });
  const reset = useMutation({
    mutationFn: () => write<ChainView>("POST", paths.apiRepoPanelChainReset(instance, project), {}),
    onMutate: () => setNote(""),
    onSuccess: (next) => done(next, ""),
  });
  const off = useMutation({
    mutationFn: () => write<ChainView>("POST", paths.apiRepoPanelChainOff(instance, project), {}),
    onMutate: () => setNote(""),
    onSuccess: (next) => done(next, ""),
  });
  const clear = useClearMark(key);
  const busy = save.isPending || reset.isPending || off.isPending;
  const c = chain.data;
  const mode = starting ? "custom" : (c?.mode ?? "inherit");
  const editing = !!c && !c.readOnly && mode === "custom";
  const pick = (to: string) => {
    if (!c || to === mode) return;
    setNote("");
    if (to === "custom") setStarting(true);
    else if (to === "off") off.mutate();
    else if (starting && c.mode === "inherit") setStarting(false);
    else if (c.mode !== "custom" || window.confirm("Drop this repository's own chain B? Its panel then uses its owner team's chain B, or the global one.")) {
      reset.mutate();
    }
  };
  return (
    <section className="card" aria-label="Chain B (review panel)">
      <h2>Chain B (review panel)</h2>
      <div className="pad meta">
        With a chain B, each review also runs on its first entry, and the two results are put together. Without one there is no
        panel.
      </div>
      {chain.isPending && <Loading />}
      {chain.isError && <ErrorBox error={chain.error} />}
      {c && (
        <>
          {!c.readOnly && (
            <div className="pad actions-row" role="group" aria-label="Chain B mode">
              {(["inherit", "custom", "off"] as const).map((m) => (
                <button
                  key={m}
                  type="button"
                  className={m === mode ? "btn small primary" : "btn small"}
                  aria-pressed={m === mode}
                  disabled={busy}
                  onClick={() => pick(m)}
                >
                  {m === "inherit" ? "Inherit" : m === "custom" ? "Custom" : "Off"}
                </button>
              ))}
            </div>
          )}
          <div className="pad meta">{starting ? "Choose the entries of its own chain B." : modeText(c)}</div>
          {mode !== "off" && (
            <ChainEditor
              // A new answer from the server, or the start of an own chain, starts the editor again.
              key={`${JSON.stringify(c)}:${editing}`}
              label="Panel chain"
              value={starting ? { ...c, entries: [] } : c}
              choices={editing ? c.choices : []}
              readOnly={!editing}
              busy={busy}
              onSave={(entries) => save.mutate(entries)}
              onClear={(e) => clear.mutate(e)}
            />
          )}
          {c.readOnly && <div className="pad meta">Only admins and maintainers of the repository's owner team change its chain B.</div>}
        </>
      )}
      {note && (
        <div className="pad note" role="status">
          {note}
        </div>
      )}
      {save.isError && <ErrorBox error={save.error} />}
      {reset.isError && <ErrorBox error={reset.error} />}
      {off.isError && <ErrorBox error={off.error} />}
      {clear.isError && <ErrorBox error={clear.error} />}
    </section>
  );
}
