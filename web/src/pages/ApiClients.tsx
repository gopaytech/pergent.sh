import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";
import { Link } from "react-router";

import { api, paths, type APIClientCreated, type APIClientInput, type APIClientRow } from "../api";
import { useWrite } from "../layout/write";
import OnceSecret from "../OnceSecret";
import { ErrorBox, Loading } from "../ui";
import { ClientFields, type ClientFieldsValue } from "./ApiClientFields";

export const clientsKey = ["api-clients"];

// ApiClients lists the API clients of the review API and makes new ones. An
// admin's page: the route is behind AdminOnly.
export default function ApiClients() {
  const [adding, setAdding] = useState(false);
  const clients = useQuery({
    queryKey: clientsKey,
    queryFn: () => api.get<{ items: APIClientRow[] }>("/api/admin/api-clients"),
  });
  return (
    <div className="narrow">
      <div className="top">
        <div>
          <h1>API clients</h1>
          <div className="sub">
            A client is a system that sends changes to pergent.sh to review. It has its own skills, model, keys and limits.
          </div>
        </div>
        {!adding && (
          <button type="button" className="btn primary" onClick={() => setAdding(true)}>
            New client
          </button>
        )}
      </div>
      {adding && <NewClient onClose={() => setAdding(false)} />}
      {clients.isPending && <Loading />}
      {clients.isError && <ErrorBox error={clients.error} />}
      {clients.isSuccess &&
        (clients.data.items.length === 0 ? (
          <div className="card empty">No API clients yet. Make one to let a system send changes for review.</div>
        ) : (
          <div className="card scroll">
            <table className="table">
              <thead>
                <tr>
                  <th>Client</th>
                  <th>Keys</th>
                  <th>Reviews this month</th>
                  <th>Failed this month</th>
                  <th>Tokens this month</th>
                </tr>
              </thead>
              <tbody>
                {clients.data.items.map((c) => (
                  <tr key={c.name}>
                    <td>
                      <Link to={paths.clientPage(c.name)}>{c.name}</Link>
                    </td>
                    <td>{c.keys}</td>
                    <td>{c.reviewsThisMonth}</td>
                    <td>{c.failedThisMonth}</td>
                    <td>{c.tokensThisMonth.toLocaleString("en-US")}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        ))}
    </div>
  );
}

// NewClient makes a client and shows its callback secret once. Limits are
// left out of the body, so the client gets the defaults; they are changed on
// its page.
function NewClient({ onClose }: { onClose: () => void }) {
  const client = useQueryClient();
  const write = useWrite();
  const [name, setName] = useState("");
  const [fields, setFields] = useState<ClientFieldsValue>({ skills: [], modelId: "", callbackUrl: "" });
  const create = useMutation({
    mutationFn: (body: APIClientInput) => write<APIClientCreated>("POST", "/api/admin/api-clients", body),
    // The answer holds the secret: it goes with the box, not after 5 minutes.
    gcTime: 0,
    onSuccess: () => client.invalidateQueries({ queryKey: clientsKey }),
  });
  const submit = (e: FormEvent) => {
    e.preventDefault();
    create.mutate({ name: name.trim(), skills: fields.skills, modelId: fields.modelId, callbackUrl: fields.callbackUrl.trim() });
  };
  const done = () => {
    create.reset();
    onClose();
  };

  if (create.isSuccess) {
    const made = create.data.client.name;
    return (
      <OnceSecret title="New API client" what="callback secret" secret={create.data.callbackSecret} onDone={done}>
        <p>This is the callback secret of {made}: it signs the callbacks pergent.sh sends. Make its first key on its page.</p>
        <p>
          <Link to={paths.clientPage(made)}>Open {made}</Link>
        </p>
      </OnceSecret>
    );
  }
  return (
    <form className="card pad stack" aria-label="New API client form" onSubmit={submit}>
      <label htmlFor="client-name">Name</label>
      <input
        id="client-name"
        type="text"
        autoComplete="off"
        autoCapitalize="none"
        spellCheck={false}
        maxLength={63}
        value={name}
        onChange={(e) => setName(e.target.value)}
        required
      />
      <div className="meta">1 to 63 characters: a-z, 0-9 and -. It never changes.</div>
      <ClientFields value={fields} onChange={setFields} />
      {create.isError && <ErrorBox error={create.error} />}
      <div className="actions-row">
        <button type="submit" className="btn primary" disabled={create.isPending}>
          Create client
        </button>
        <button type="button" className="btn" onClick={onClose}>
          Cancel
        </button>
      </div>
    </form>
  );
}
