import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useCallback, useRef, useState, type FormEvent } from "react";
import { Link, useNavigate, useParams } from "react-router";

import {
  api,
  isStatus,
  paths,
  type APIClientEdit,
  type APIClientView,
  type APIKeyCreated,
  type APIKeyView,
  type APILimits,
} from "../api";
import { useWrite } from "../layout/write";
import OnceSecret from "../OnceSecret";
import { ErrorBox, Loading, NotFound, Tabs, utcText } from "../ui";
import ClientReviews from "./ApiClientReviews";
import { clientsKey } from "./ApiClients";
import { ClientFields, inLibraryOrder, type ClientFieldsValue } from "./ApiClientFields";

type Tab = "reviews" | "settings" | "keys";

const clientKey = (name: string) => ["api-client", name];

// Shown is a secret the server made and will not show again, kept by the
// page until the admin closes it: a tab switch must not lose it.
type Shown = { id: number; title: string; what: string; secret: string; note?: string };

// ApiClient is one API client: its reviews, settings and keys. An admin's
// page.
export default function ApiClient() {
  const { name = "" } = useParams();
  const client = useQuery({
    queryKey: clientKey(name),
    queryFn: () => api.get<APIClientView>(paths.apiAdminClient(name)),
  });
  // Only a page with nothing to show gives way to loading, not found or an
  // error: a read that fails behind a page keeps it, and any secret on it.
  if (!client.data) {
    if (client.isPending) return <Loading />;
    if (isStatus(client.error, 404)) return <NotFound />;
    return <ErrorBox error={client.error} />;
  }
  // The key resets the page's state (the tab, the secrets shown, the
  // settings form) when the route moves to another client.
  return <ClientPage key={client.data.name} client={client.data} refreshError={client.isError ? client.error : null} />;
}

function ClientPage({ client, refreshError }: { client: APIClientView; refreshError: unknown }) {
  const [tab, setTab] = useState<Tab>("reviews");
  const [shown, setShown] = useState<Shown[]>([]);
  const nextID = useRef(0);
  const show = useCallback((s: Omit<Shown, "id">) => {
    const id = nextID.current++;
    setShown((list) => [...list, { ...s, id }]);
  }, []);
  return (
    <div className="narrow">
      <div className="top">
        <div>
          <h1>{client.name}</h1>
          <div className="sub">
            <Link to="/api-clients">API clients</Link>
          </div>
        </div>
      </div>
      {refreshError !== null && <ErrorBox error={refreshError} />}
      {shown.map((s) => (
        <OnceSecret
          key={s.id}
          title={s.title}
          what={s.what}
          secret={s.secret}
          onDone={() => setShown((list) => list.filter((x) => x.id !== s.id))}
        >
          {s.note && <p>{s.note}</p>}
        </OnceSecret>
      ))}
      <Tabs
        tabs={[
          ["reviews", "Reviews"],
          ["settings", "Settings"],
          ["keys", "Keys"],
        ]}
        value={tab}
        onChange={setTab}
      />
      <div role="tabpanel">
        {tab === "reviews" && <ClientReviews client={client.name} />}
        {tab === "settings" && <Settings client={client} show={show} />}
        {tab === "keys" && <Keys client={client} show={show} />}
      </div>
    </div>
  );
}

// limitFields are the limits in the order they are shown: the key in the
// body, its label and what it caps.
const limitFields: [keyof APILimits, string, string][] = [
  ["maxRequestBytes", "Max request bytes", "1024 to 819200. The size of the files in one request."],
  ["maxFiles", "Max files", "1 to 1000. The files in one request."],
  ["maxInFlight", "Max in flight", "1 to 50. Reviews of this client running or queued at once; more wait in the queue."],
  ["requestsPerHour", "Requests per hour", "1 to 100000. Per key."],
  ["monthlyTokenBudget", "Monthly token budget", "Tokens in and out for a UTC month. A request over it is refused."],
];

function Settings({ client, show }: { client: APIClientView; show: (s: Omit<Shown, "id">) => void }) {
  return (
    <>
      <SettingsForm client={client} />
      <CallbackSecret client={client} show={show} />
      <RemoveClient client={client} />
    </>
  );
}

// SettingsForm changes the skills, model, callback URL and limits. The name
// never changes.
function SettingsForm({ client }: { client: APIClientView }) {
  const cache = useQueryClient();
  const write = useWrite();
  const [fields, setFields] = useState<ClientFieldsValue>({
    skills: client.skills,
    modelId: client.modelId,
    callbackUrl: client.callbackUrl,
  });
  const [limits, setLimits] = useState<Record<keyof APILimits, string>>(() => {
    const l = client.limits;
    return {
      maxRequestBytes: String(l.maxRequestBytes),
      maxFiles: String(l.maxFiles),
      maxInFlight: String(l.maxInFlight),
      requestsPerHour: String(l.requestsPerHour),
      monthlyTokenBudget: String(l.monthlyTokenBudget),
    };
  });
  const [note, setNote] = useState("");
  const library = useQuery({ queryKey: ["skills"], queryFn: () => api.get<{ name: string }[]>("/api/skills") });
  const save = useMutation({
    mutationFn: (body: APIClientEdit) => write<APIClientView>("PUT", paths.apiAdminClient(client.name), body),
    onMutate: () => setNote(""),
    onSuccess: (view) => {
      cache.setQueryData(clientKey(client.name), view);
      setNote("Settings saved.");
    },
  });
  const submit = (e: FormEvent) => {
    e.preventDefault();
    save.mutate({
      skills: inLibraryOrder(fields.skills, library.data?.map((s) => s.name) ?? []),
      modelId: fields.modelId,
      callbackUrl: fields.callbackUrl.trim(),
      limits: {
        maxRequestBytes: Number(limits.maxRequestBytes),
        maxFiles: Number(limits.maxFiles),
        maxInFlight: Number(limits.maxInFlight),
        requestsPerHour: Number(limits.requestsPerHour),
        monthlyTokenBudget: Number(limits.monthlyTokenBudget),
      },
    });
  };
  return (
    <form className="card pad stack" aria-label="Settings" onSubmit={submit}>
      <ClientFields value={fields} onChange={setFields} />
      <h3 className="group">Limits</h3>
      {limitFields.map(([key, label, hint]) => (
        <div key={key}>
          <label htmlFor={`limit-${key}`}>{label}</label>
          <input
            id={`limit-${key}`}
            type="number"
            min={1}
            step={1}
            value={limits[key]}
            onChange={(e) => setLimits({ ...limits, [key]: e.target.value })}
            required
          />
          <div className="meta">{hint}</div>
        </div>
      ))}
      {save.isError && <ErrorBox error={save.error} />}
      {note && (
        <div className="note" role="status">
          {note}
        </div>
      )}
      <div className="actions-row">
        <button type="submit" className="btn primary" disabled={save.isPending}>
          Save settings
        </button>
      </div>
    </form>
  );
}

// CallbackSecret makes a new callback secret and hands it to the page to
// show once. The old one keeps signing for a while, so the caller can switch
// over.
function CallbackSecret({ client, show }: { client: APIClientView; show: (s: Omit<Shown, "id">) => void }) {
  const cache = useQueryClient();
  const write = useWrite();
  const rotate = useMutation({
    // The secret goes to the page, not into the mutation's state: the page
    // outlives this tab, and the answer is not kept after it.
    mutationFn: async () => {
      const made = await write<{ callbackSecret: string }>("POST", paths.apiAdminClientSecret(client.name));
      show({ title: "New callback secret", what: "callback secret", secret: made.callbackSecret });
    },
    onSuccess: () => cache.invalidateQueries({ queryKey: clientKey(client.name) }),
  });
  const confirmRotate = () => {
    if (window.confirm(`Make a new callback secret for ${client.name}? The old one keeps signing for a day, so the caller can switch over.`)) {
      rotate.mutate();
    }
  };
  return (
    <>
      <section className="card pad stack" aria-label="Callback secret">
        <h2>Callback secret</h2>
        <div className="meta">
          It signs every callback pergent.sh sends to this client. The secret is shown only when it is made.
        </div>
        {client.previousSecretUntil && (
          <div className="note">The old secret also works until {utcText(client.previousSecretUntil)}.</div>
        )}
        {rotate.isError && <ErrorBox error={rotate.error} />}
        <div className="actions-row">
          <button type="button" className="btn" disabled={rotate.isPending} onClick={confirmRotate}>
            Rotate secret
          </button>
        </div>
      </section>
    </>
  );
}

// RemoveClient deletes the client after a confirmation that says what goes.
function RemoveClient({ client }: { client: APIClientView }) {
  const cache = useQueryClient();
  const write = useWrite();
  const navigate = useNavigate();
  const remove = useMutation({
    mutationFn: () => write<void>("DELETE", paths.apiAdminClient(client.name)),
    onSuccess: () => {
      navigate("/api-clients");
      cache.removeQueries({ queryKey: clientKey(client.name) });
      return cache.invalidateQueries({ queryKey: clientsKey });
    },
  });
  const confirmRemove = () => {
    if (
      window.confirm(
        `Remove the client ${client.name}? Its keys stop at once, its running reviews end with no callback, and its reviews and dismissals are deleted.`,
      )
    ) {
      remove.mutate();
    }
  };
  return (
    <section className="card pad stack" aria-label="Remove client">
      <h2>Remove client</h2>
      <div className="meta">This cannot be undone.</div>
      {remove.isError && <ErrorBox error={remove.error} />}
      <div className="actions-row">
        <button type="button" className="btn" disabled={remove.isPending} onClick={confirmRemove}>
          Remove client
        </button>
      </div>
    </section>
  );
}

// Keys lists the client's keys and makes new ones. A key is shown once, when
// it is made, by the page; the list has its prefix.
function Keys({ client, show }: { client: APIClientView; show: (s: Omit<Shown, "id">) => void }) {
  const cache = useQueryClient();
  const write = useWrite();
  const [name, setName] = useState("");
  const [expires, setExpires] = useState("");
  const create = useMutation({
    // The key goes to the page, not into the mutation's state: the page
    // outlives this tab, and the answer is not kept after it.
    mutationFn: async (body: { name: string; expiresAt: string | null }) => {
      const made = await write<APIKeyCreated>("POST", paths.apiAdminClientKeys(client.name), body);
      show({
        title: "New API key",
        what: "API key",
        secret: made.secret,
        note: `Key ${made.key.name} starts with ${made.key.prefix}. Send it as a bearer token.`,
      });
    },
    onSuccess: () => {
      setName("");
      setExpires("");
      return cache.invalidateQueries({ queryKey: clientKey(client.name) });
    },
  });
  const submit = (e: FormEvent) => {
    e.preventDefault();
    // A date is the end of that day in UTC.
    create.mutate({ name: name.trim(), expiresAt: expires === "" ? null : new Date(`${expires}T23:59:59Z`).toISOString() });
  };
  return (
    <>
      <form className="card pad inline-form" aria-label="New API key form" onSubmit={submit}>
        <div className="grow">
          <label htmlFor="key-name">Key name</label>
          <input
            id="key-name"
            type="text"
            autoComplete="off"
            spellCheck={false}
            maxLength={100}
            value={name}
            onChange={(e) => setName(e.target.value)}
            required
          />
        </div>
        <div>
          <label htmlFor="key-expires">Expires (optional)</label>
          <input id="key-expires" type="date" value={expires} onChange={(e) => setExpires(e.target.value)} />
        </div>
        <button type="submit" className="btn primary" disabled={create.isPending}>
          Create key
        </button>
        {create.isError && <ErrorBox error={create.error} />}
      </form>
      {client.keys.length === 0 ? (
        <div className="card empty">No keys yet. A caller needs one to send a review.</div>
      ) : (
        <div className="card scroll">
          <table className="table">
            <thead>
              <tr>
                <th>Name</th>
                <th>Prefix</th>
                <th>Created</th>
                <th>Expires</th>
                <th>Last used</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {client.keys.map((k) => (
                <KeyRow key={k.id} client={client.name} apiKey={k} />
              ))}
            </tbody>
          </table>
        </div>
      )}
    </>
  );
}

function KeyRow({ client, apiKey: k }: { client: string; apiKey: APIKeyView }) {
  const cache = useQueryClient();
  const write = useWrite();
  const revoke = useMutation({
    mutationFn: () => write<void>("DELETE", paths.apiAdminClientKey(client, k.id)),
    onSuccess: () => cache.invalidateQueries({ queryKey: clientKey(client) }),
  });
  const confirmRevoke = () => {
    if (window.confirm(`Revoke the key ${k.name} (${k.prefix})? A caller using it is refused at once.`)) revoke.mutate();
  };
  return (
    <tr>
      <td>{k.name}</td>
      <td className="mono">{k.prefix}</td>
      <td className="nowrap">{utcText(k.createdAt)}</td>
      <td className="nowrap">{k.expiresAt ? utcText(k.expiresAt) : "never"}</td>
      <td className="nowrap">{k.lastUsedAt ? utcText(k.lastUsedAt) : "never"}</td>
      <td>
        <button type="button" className="btn small" disabled={revoke.isPending} onClick={confirmRevoke}>
          Revoke
        </button>
        {revoke.isError && <ErrorBox error={revoke.error} />}
      </td>
    </tr>
  );
}
