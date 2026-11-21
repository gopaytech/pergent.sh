import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";

import { api, type ChainEntryView, type ChainRef, type ChainView } from "./api";
import ChainEditor from "./ChainEditor";
import { useWrite } from "./layout/write";
import { ErrorBox, Loading } from "./ui";

// PanelChainCard is a chain B of a team or of the instance: the review
// panel's second chain, edited with ChainEditor on the /panel-chain route at
// path. An empty chain B can be saved: it is no chain B. inheritNote is set
// for a team, whose chain B may be the global one shown; the editor then
// starts when the team chooses to make its own. A save the image check
// refuses shows its message.
export default function PanelChainCard({
  title,
  intro,
  path,
  queryKey,
  inheritNote,
  clearPath,
  also,
}: {
  title: string;
  intro: string;
  path: string;
  queryKey: unknown[];
  inheritNote?: string;
  // clearPath is the route that clears the mark of an entry.
  clearPath: (e: ChainEntryView) => string;
  // also are the queries a save or a clear makes stale.
  also: unknown[][];
}) {
  const client = useQueryClient();
  const write = useWrite();
  const chain = useQuery({ queryKey, queryFn: () => api.get<ChainView>(path) });
  const [starting, setStarting] = useState(false);
  const [note, setNote] = useState("");
  const stale = () => also.forEach((k) => client.invalidateQueries({ queryKey: k }));
  const save = useMutation({
    mutationFn: (entries: ChainRef[]) => write<ChainView>("PUT", path, { entries }),
    onMutate: () => setNote(""),
    onSuccess: (next) => {
      client.setQueryData(queryKey, next);
      stale();
      setStarting(false);
      setNote("Saved.");
    },
  });
  const clear = useMutation({
    mutationFn: (e: ChainEntryView) => write<void>("DELETE", clearPath(e)),
    onSuccess: () => {
      client.invalidateQueries({ queryKey });
      stale();
    },
  });
  const c = chain.data;
  const inherited = !!inheritNote && !!c && c.source !== "team";
  return (
    <section className="card" aria-label={title}>
      <h2>{title}</h2>
      <div className="pad meta">{intro}</div>
      {chain.isPending && <Loading />}
      {chain.isError && <ErrorBox error={chain.error} />}
      {c && inherited && (
        <>
          <div className="pad meta">{inheritNote}</div>
          {!starting && (
            <>
              <ChainEditor key={JSON.stringify(c)} label="Panel chain" value={c} choices={[]} readOnly onSave={() => {}} onClear={(e) => clear.mutate(e)} />
              {!c.readOnly && (
                <div className="pad">
                  <button type="button" className="btn small" onClick={() => setStarting(true)}>
                    Make a chain B
                  </button>
                </div>
              )}
            </>
          )}
          {starting && (
            <ChainEditor
              label="Panel chain"
              value={{ ...c, entries: [] }}
              choices={c.choices}
              readOnly={false}
              busy={save.isPending}
              onSave={(entries) => save.mutate(entries)}
              onClear={(e) => clear.mutate(e)}
            />
          )}
        </>
      )}
      {c && !inherited && (
        <ChainEditor
          // A new answer from the server starts the editor again from it.
          key={JSON.stringify(c)}
          label="Panel chain"
          value={c}
          choices={c.choices}
          readOnly={c.readOnly}
          allowEmpty
          busy={save.isPending}
          onSave={(entries) => save.mutate(entries)}
          onClear={(e) => clear.mutate(e)}
        />
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
