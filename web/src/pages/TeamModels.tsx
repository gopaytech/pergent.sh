import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";

import { Button } from "@/components/ui/button";
import { Card, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { cn } from "@/lib/utils";

import { api, paths, type ChainEntryView, type ChainRef, type ChainView, type TeamModelsView, type TeamModelView } from "../api";
import ChainEditor from "../ChainEditor";
import { useWrite } from "../layout/write";
import MarkBanner, { markText } from "../MarkBanner";
import { AddModelForm, EditModelForm, windowText } from "../ModelForms";
import { ErrorBox, Loading, secretText } from "../ui";

// muted is a small grey line under a model's name.
const muted = "text-muted-foreground text-xs";
// pad is the room around a line in a card.
const pad = "px-4 py-3";

// TeamModelsTab is the Models tab of a team's page: the team's own models
// and keys, and its chain. Every member sees them (never a key, and a viewer
// no base URL); a maintainer of the team, or an admin, changes them. The
// team's entries run only for repositories the team owns.
export default function TeamModelsTab({ team }: { team: number }) {
  const models = useQuery({
    queryKey: ["team-models", team],
    queryFn: () => api.get<TeamModelsView>(paths.apiTeamModels(team)),
  });
  if (models.isPending) return <Loading />;
  if (models.isError) return <ErrorBox error={models.error} />;
  const v = models.data;
  return (
    <>
      <Card role="region" aria-label="Models" className="gap-0 py-0">
        <CardHeader className="border-b py-4">
          <CardTitle>Models</CardTitle>
          <CardDescription>
            The team's own models and keys. They run only for repositories this team owns, and only where a chain names them.
          </CardDescription>
        </CardHeader>
        <MarkBanner entries={v.models} />
        {v.models.length === 0 && <div className="text-muted-foreground px-4 py-3 text-sm">No model of the team's own yet.</div>}
        {v.models.map((m) => (
          <TeamModelRow key={m.id} team={team} m={m} canEdit={v.canEdit} windowOff={v.windowOff} />
        ))}
      </Card>
      <TeamChain team={team} />
      {v.canEdit && <AddTeamModel team={team} windowOff={v.windowOff} />}
    </>
  );
}

// useTeamChanged reads again what a change of a team's entries moves: its
// list, its chain and the repository chains.
function useTeamChanged(team: number) {
  const client = useQueryClient();
  return () => {
    client.invalidateQueries({ queryKey: ["team-models", team] });
    client.invalidateQueries({ queryKey: ["team-limits", team] });
    client.invalidateQueries({ queryKey: ["team-chain", team] });
    client.invalidateQueries({ queryKey: ["repo-chain"] });
    client.invalidateQueries({ queryKey: ["repo-reviewers"] });
  };
}

function AddTeamModel({ team, windowOff }: { team: number; windowOff: string }) {
  const changed = useTeamChanged(team);
  return <AddModelForm<TeamModelView> path={paths.apiTeamModels(team)} windowOff={windowOff} onAdded={changed} />;
}

function TeamModelRow({ team, m, canEdit, windowOff }: { team: number; m: TeamModelView; canEdit: boolean; windowOff: string }) {
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
    <div className="flex flex-col gap-1 border-b px-4 py-3 text-sm last:border-b-0" role="group" aria-label={m.name}>
      <b className="font-semibold">{m.name}</b>
      <div className="text-muted-foreground font-mono text-xs">
        {m.provider} / {m.model}
      </div>
      {windowText(m.contextWindow, m.maxOutput) !== "" && (
        <div className={muted} title="Context window / Max output">
          {windowText(m.contextWindow, m.maxOutput)}
        </div>
      )}
      {m.baseURL && <div className={muted}>Base URL {m.baseURL}</div>}
      <div className={muted}>Key {secretText(m.key)}</div>
      {m.usedIn.length > 0 && <div className={muted}>In chains: {m.usedIn.join(", ")}</div>}
      {m.mark?.live && (
        <div className="flex items-center gap-3">
          <span className="text-warn min-w-0 grow text-xs">Marked: {markText(m.mark)}</span>
          {canEdit && (
            <Button type="button" variant="outline" size="sm" aria-label={`Clear mark of ${m.name}`} disabled={clear.isPending} onClick={() => clear.mutate()}>
              Clear mark
            </Button>
          )}
        </div>
      )}
      {canEdit && !editing && (
        <div className="mt-2 flex flex-wrap items-center gap-2">
          <Button type="button" variant="outline" size="sm" aria-label={`Edit ${m.name}`} disabled={remove.isPending} onClick={() => setEditing(true)}>
            Edit
          </Button>
          <Button type="button" variant="destructive-outline" size="sm" aria-label={`Remove ${m.name}`} disabled={remove.isPending} onClick={confirmRemove}>
            Remove
          </Button>
        </div>
      )}
      {editing && <EditModelForm m={m} path={paths.apiTeamModel(team, m.id)} windowOff={windowOff} onDone={() => setEditing(false)} onSaved={changed} />}
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
      client.invalidateQueries({ queryKey: ["repo-reviewers"] });
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
    <Card role="region" aria-label="Team chain" className="gap-0 py-0">
      <CardHeader className="border-b py-4">
        <CardTitle>Team chain</CardTitle>
        <CardDescription>
          Reviews of the repositories this team owns try these entries in order, unless a repository has its own chain. When the
          provider says an entry is out of quota or credit, or rejects its key, the review starts again on the next one.
        </CardDescription>
      </CardHeader>
      {chain.isPending && <Loading />}
      {chain.isError && (
        <div className={pad}>
          <ErrorBox error={chain.error} />
        </div>
      )}
      {chain.isSuccess && !own && (
        <>
          <div className={cn(pad, "text-muted-foreground text-sm")}>This team has no chain of its own: its repositories use the global chain.</div>
          {!starting && (
            <>
              <ChainEditor key={JSON.stringify(chain.data)} value={chain.data} choices={[]} readOnly onSave={() => {}} onClear={(e) => clear.mutate(e)} />
              {!chain.data.readOnly && (
                <div className={pad}>
                  <Button type="button" variant="outline" size="sm" onClick={() => setStarting(true)}>
                    Make a team chain
                  </Button>
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
              onCancel={() => setStarting(false)}
              onSave={(entries) => save.mutate(entries)}
              onClear={(e) => clear.mutate(e)}
            />
          )}
        </>
      )}
      {chain.isSuccess && own && (
        <>
          {!chain.data.readOnly && <div className={cn(pad, "text-muted-foreground text-sm")}>Save it with no entries to use the global chain again.</div>}
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
        <div className={cn(pad, "text-ok text-sm")} role="status">
          {note}
        </div>
      )}
      {save.isError && (
        <div className={pad}>
          <ErrorBox error={save.error} />
        </div>
      )}
      {clear.isError && (
        <div className={pad}>
          <ErrorBox error={clear.error} />
        </div>
      )}
    </Card>
  );
}
