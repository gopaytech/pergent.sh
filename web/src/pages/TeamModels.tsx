import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import { Link, useParams } from "react-router";

import { api, isStatus, paths, type ChainEntryView, type ChainRef, type ChainView, type TeamModelsView, type TeamModelView } from "../api";
import ChainEditor from "../ChainEditor";
import { useWrite } from "../layout/write";
import MarkBanner, { markText } from "../MarkBanner";
import { AddModelForm, EditModelForm } from "../ModelForms";
import { ErrorBox, Loading, NotFound, secretText } from "../ui";

// TeamModels is a team's page in the user area: the team's own models and
// keys, and its chain. Every member sees them (never a key, and a viewer no
// base URL); a maintainer of the team, or an admin, changes them. The team's
// entries run only for repositories the team owns.
export default function TeamModels() {
  const params = useParams();
  const id = /^[1-9]\d{0,15}$/.test(params.id ?? "") ? Number(params.id) : null;
  const models = useQuery({
    queryKey: ["team-models", id],
    queryFn: () => api.get<TeamModelsView>(paths.apiTeamModels(id!)),
    enabled: id !== null,
  });
  if (id === null || isStatus(models.error, 404)) return <NotFound />;
  if (models.isPending) return <Loading />;
  if (models.isError) return <ErrorBox error={models.error} />;
  const v = models.data;
  return (
    <div className="narrow">
      <div className="crumbs">
        <Link to="/teams">Teams</Link> / {v.team.name}
      </div>
      <h1>{v.team.name}</h1>
      <div className="sub spaced">Your role: {v.role}</div>
      <section className="card" aria-label="Models">
        <h2>Models</h2>
        <MarkBanner entries={v.models} />
        <div className="pad meta">
          The team's own models and keys. They run only for repositories this team owns, and only where a chain names them.
        </div>
        {v.models.length === 0 && <div className="pad meta">No model of the team's own yet.</div>}
        {v.models.map((m) => (
          <TeamModelRow key={m.id} team={v.team.id} m={m} canEdit={v.canEdit} />
        ))}
      </section>
      <TeamChain team={v.team.id} />
      {v.canEdit && <AddTeamModel team={v.team.id} />}
    </div>
  );
}

// useTeamChanged reads again what a change of a team's entries moves: its
// list, its chain and the repository chains.
function useTeamChanged(team: number) {
  const client = useQueryClient();
  return () => {
    client.invalidateQueries({ queryKey: ["team-models", team] });
    client.invalidateQueries({ queryKey: ["team-chain", team] });
    client.invalidateQueries({ queryKey: ["repo-chain"] });
  };
}

function AddTeamModel({ team }: { team: number }) {
  const changed = useTeamChanged(team);
  return <AddModelForm<TeamModelView> path={paths.apiTeamModels(team)} onAdded={changed} />;
}

function TeamModelRow({ team, m, canEdit }: { team: number; m: TeamModelView; canEdit: boolean }) {
  const write = useWrite();
  const changed = useTeamChanged(team);
  const [editing, setEditing] = useState(false);
  const remove = useMutation({ mutationFn: () => write<void>("DELETE", paths.apiTeamModel(team, m.id)), onSuccess: changed });
  const clear = useMutation({ mutationFn: () => write<void>("DELETE", paths.apiTeamModelMark(team, m.id)), onSuccess: changed });
  const confirmRemove = () => {
    const chains = m.usedIn.length > 0 ? `, and it is taken out of these chains: ${m.usedIn.join(", ")}` : "";
    if (window.confirm(`Remove ${m.name}? Its key is deleted${chains}.`)) remove.mutate();
  };
  return (
    <div className="pad stack" role="group" aria-label={m.name}>
      <div className="row">
        <b className="grow">{m.name}</b>
      </div>
      <div className="meta mono">
        {m.provider} / {m.model}
      </div>
      {m.baseURL && <div className="meta">Base URL {m.baseURL}</div>}
      <div className="meta">Key {secretText(m.key)}</div>
      {m.usedIn.length > 0 && <div className="meta">In chains: {m.usedIn.join(", ")}</div>}
      {m.mark?.live && (
        <div className="row">
          <span className="grow meta warn-text">Marked: {markText(m.mark)}</span>
          {canEdit && (
            <button type="button" className="btn small" aria-label={`Clear mark of ${m.name}`} disabled={clear.isPending} onClick={() => clear.mutate()}>
              Clear mark
            </button>
          )}
        </div>
      )}
      {canEdit && !editing && (
        <div className="actions-row">
          <button type="button" className="btn small" aria-label={`Edit ${m.name}`} disabled={remove.isPending} onClick={() => setEditing(true)}>
            Edit
          </button>
          <button type="button" className="btn small" aria-label={`Remove ${m.name}`} disabled={remove.isPending} onClick={confirmRemove}>
            Remove
          </button>
        </div>
      )}
      {editing && <EditModelForm m={m} path={paths.apiTeamModel(team, m.id)} onDone={() => setEditing(false)} onSaved={changed} />}
      {remove.isError && <ErrorBox error={remove.error} />}
      {clear.isError && <ErrorBox error={clear.error} />}
    </div>
  );
}

// TeamChain is the chain the team's repositories use, unless a repository
// has its own. While the team has none, the server shows the global chain
// they use instead; a team chain then starts from no entries.
function TeamChain({ team }: { team: number }) {
  const client = useQueryClient();
  const write = useWrite();
  const key = ["team-chain", team];
  const chain = useQuery({ queryKey: key, queryFn: () => api.get<ChainView>(paths.apiTeamChain(team)) });
  const [starting, setStarting] = useState(false);
  const [note, setNote] = useState("");
  const save = useMutation({
    mutationFn: (entries: ChainRef[]) => write<ChainView>("PUT", paths.apiTeamChain(team), { entries }),
    onMutate: () => setNote(""),
    onSuccess: (next) => {
      client.setQueryData(key, next);
      client.invalidateQueries({ queryKey: ["team-models", team] });
      client.invalidateQueries({ queryKey: ["repo-chain"] });
      setStarting(false);
      setNote("Saved.");
    },
  });
  // A team entry's mark is the team's to clear; a global entry's, an admin's.
  const clear = useMutation({
    mutationFn: (e: ChainEntryView) =>
      write<void>("DELETE", e.source === "team" ? paths.apiTeamModelMark(team, e.id) : paths.apiAdminModelMark(e.id)),
    onSuccess: () => {
      client.invalidateQueries({ queryKey: key });
      client.invalidateQueries({ queryKey: ["team-models", team] });
    },
  });
  const own = chain.data?.source === "team";
  return (
    <section className="card" aria-label="Team chain">
      <h2>Team chain</h2>
      <div className="pad meta">
        Reviews of the repositories this team owns try these entries in order, unless a repository has its own chain. When the
        provider says an entry is out of quota or credit, or rejects its key, the review starts again on the next one.
      </div>
      {chain.isPending && <Loading />}
      {chain.isError && <ErrorBox error={chain.error} />}
      {chain.isSuccess && !own && (
        <>
          <div className="pad meta">This team has no chain of its own: its repositories use the global chain.</div>
          {!starting && (
            <>
              <ChainEditor key={JSON.stringify(chain.data)} value={chain.data} choices={[]} readOnly onSave={() => {}} onClear={(e) => clear.mutate(e)} />
              {!chain.data.readOnly && (
                <div className="pad">
                  <button type="button" className="btn small" onClick={() => setStarting(true)}>
                    Make a team chain
                  </button>
                </div>
              )}
            </>
          )}
          {starting && (
            <ChainEditor
              value={{ ...chain.data, entries: [] }}
              choices={chain.data.choices}
              readOnly={false}
              busy={save.isPending}
              onSave={(entries) => save.mutate(entries)}
              onClear={(e) => clear.mutate(e)}
            />
          )}
        </>
      )}
      {chain.isSuccess && own && (
        <>
          {!chain.data.readOnly && <div className="pad meta">Save it with no entries to use the global chain again.</div>}
          <ChainEditor
            // A new answer from the server starts the editor again from it.
            key={JSON.stringify(chain.data)}
            value={chain.data}
            choices={chain.data.choices}
            readOnly={chain.data.readOnly}
            allowEmpty
            busy={save.isPending}
            onSave={(entries) => save.mutate(entries)}
            onClear={(e) => clear.mutate(e)}
          />
        </>
      )}
      {note && (
        <div className="pad note" role="status">
          {note}
        </div>
      )}
      {save.isError && <ErrorBox error={save.error} />}
      {clear.isError && <ErrorBox error={clear.error} />}
    </section>
  );
}
