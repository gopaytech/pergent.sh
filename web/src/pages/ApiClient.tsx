import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useCallback, useRef, useState, type FormEvent } from "react";
import { Link, useNavigate, useParams } from "react-router";

import { Alert } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";

import {
  api,
  isStatus,
  paths,
  type APIClientEdit,
  type APICallbackTest,
  type APIClientView,
  type APIKeyCreated,
  type APIKeyView,
  type APILimits,
} from "../api";
import { useWrite } from "../layout/write";
import OnceSecret from "../OnceSecret";
import { ErrorBox, Loading, NotFound, PageHeader, Tabs, utcText } from "../ui";
import ClientReviews from "./ApiClientReviews";
import { clientsKey } from "./ApiClients";
import { ClientFields, inLibraryOrder, type ClientFieldsValue } from "./ApiClientFields";
import { useUrlParam } from "../urlState";

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
  const [tab, setTab] = useUrlParam<Tab>("tab", "reviews", ["reviews", "settings", "keys"]);
  const [shown, setShown] = useState<Shown[]>([]);
  const nextID = useRef(0);
  const show = useCallback((s: Omit<Shown, "id">) => {
    const id = nextID.current++;
    setShown((list) => [...list, { ...s, id }]);
  }, []);
  return (
    <div className="max-w-[1100px]">
      <PageHeader
        title={client.name}
        description={
          <Link className="text-muted-foreground underline-offset-4 hover:underline" to="/api-clients">
            API clients
          </Link>
        }
      />
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
  ["maxRequestBytes", "Max request bytes", "1024 to 819200. The size of the whole JSON request body."],
  ["maxFiles", "Max files", "1 to 1000. The files in one request."],
  ["maxInFlight", "Max in flight", "1 to 50. Reviews of this client running at once; more wait in the queue."],
  ["requestsPerHour", "Requests per hour", "1 to 100000. Per key."],
  ["monthlyTokenBudget", "Monthly token budget", "Tokens per calendar month (UTC): input, cache read, cache write, output and reasoning. A request over it is refused."],
];

function Settings({ client, show }: { client: APIClientView; show: (s: Omit<Shown, "id">) => void }) {
  return (
    <>
      <SettingsForm client={client} />
      <CallbackSecret client={client} show={show} />
      <TestCallback client={client} />
      <RemoveClient client={client} />
    </>
  );
}

// formCard is a card that holds a form of stacked fields.
const formCard = "mb-4 gap-4 px-4 py-4";

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
    <form aria-label="Settings" onSubmit={submit}>
      <Card className={formCard}>
        <ClientFields value={fields} onChange={setFields} />
        <h3 className="text-muted-foreground m-0 text-[13px] font-semibold">Limits</h3>
        {limitFields.map(([key, label, hint]) => (
          <div className="grid gap-2" key={key}>
            <Label htmlFor={`limit-${key}`}>{label}</Label>
            <Input
              id={`limit-${key}`}
              className="max-w-64"
              type="number"
              min={1}
              step={1}
              value={limits[key]}
              onChange={(e) => setLimits({ ...limits, [key]: e.target.value })}
              required
            />
            <div className="text-muted-foreground text-sm">{hint}</div>
          </div>
        ))}
        {save.isError && <ErrorBox error={save.error} />}
        {note && (
          <div className="text-ok text-sm" role="status">
            {note}
          </div>
        )}
        <div className="flex gap-2">
          <Button type="submit" disabled={save.isPending}>
            Save settings
          </Button>
        </div>
      </Card>
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
    <Card role="region" aria-label="Callback secret" className="mb-4">
      <CardHeader>
        <CardTitle>Callback secret</CardTitle>
        <CardDescription>It signs every callback pergent.sh sends to this client. The secret is shown only when it is made.</CardDescription>
      </CardHeader>
      <CardContent className="grid justify-items-start gap-3">
        {client.previousSecretUntil && (
          <div className="text-ok text-sm">The old secret also works until {utcText(client.previousSecretUntil)}.</div>
        )}
        {rotate.isError && <ErrorBox error={rotate.error} />}
        <Button type="button" variant="outline" size="sm" disabled={rotate.isPending} onClick={confirmRotate}>
          Rotate secret
        </Button>
      </CardContent>
    </Card>
  );
}

// answerOf says what a test callback got. Only a 2xx answer without an error
// is a delivery: any other is shown as a failure, and a callback that got no
// answer says why.
function answerOf(a: APICallbackTest): { ok: boolean; text: string } {
  if (a.statusCode === 0) return { ok: false, text: `No answer: ${a.error}` };
  const took = `Answered ${a.statusCode} in ${a.durationMs} ms`;
  if (a.error) return { ok: false, text: `${took}. ${a.error}` };
  if (a.statusCode < 200 || a.statusCode >= 300) return { ok: false, text: `${took}. Only a 2xx answer is a delivery.` };
  return { ok: true, text: took };
}

// TestCallback sends the client's saved callback URL a test review, signed as
// a real callback is. The review says test: true and nothing_reviewed, so a
// receiver can't take it for a pass.
function TestCallback({ client }: { client: APIClientView }) {
  const write = useWrite();
  const send = useMutation({
    mutationFn: () => write<APICallbackTest>("POST", paths.apiAdminClientTestCallback(client.name)),
  });
  const answer = send.data ? answerOf(send.data) : null;
  return (
    <Card role="region" aria-label="Test callback" className="mb-4">
      <CardHeader>
        <CardTitle>Test callback</CardTitle>
        <CardDescription>
          Sends the saved callback URL a test review, signed like a real one. Its verdict is nothing reviewed and it says it is a test.
        </CardDescription>
      </CardHeader>
      <CardContent className="grid justify-items-start gap-3">
        {!client.callbackUrl && <div className="text-muted-foreground text-sm">Save a callback URL first.</div>}
        {send.isError && <ErrorBox error={send.error} />}
        {answer &&
          (answer.ok ? (
            <div className="text-ok text-sm" role="status">
              {answer.text}
            </div>
          ) : (
            <Alert variant="destructive">{answer.text}</Alert>
          ))}
        <Button type="button" variant="outline" size="sm" disabled={send.isPending || !client.callbackUrl} onClick={() => send.mutate()}>
          Send test callback
        </Button>
      </CardContent>
    </Card>
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
    <Card role="region" aria-label="Remove client" className="mb-4">
      <CardHeader>
        <CardTitle>Remove client</CardTitle>
        <CardDescription>This cannot be undone.</CardDescription>
      </CardHeader>
      <CardContent className="grid justify-items-start gap-3">
        {remove.isError && <ErrorBox error={remove.error} />}
        <Button type="button" variant="destructive-outline" size="sm" disabled={remove.isPending} onClick={confirmRemove}>
          Remove client
        </Button>
      </CardContent>
    </Card>
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
      <form aria-label="New API key form" onSubmit={submit}>
        <Card className="mb-4 flex-row flex-wrap items-end gap-3 px-4 py-4">
          <div className="grid min-w-48 flex-1 gap-2">
            <Label htmlFor="key-name">Key name</Label>
            <Input
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
          <div className="grid gap-2">
            <Label htmlFor="key-expires">Expires (optional)</Label>
            <Input id="key-expires" className="w-44" type="date" value={expires} onChange={(e) => setExpires(e.target.value)} />
          </div>
          <Button type="submit" disabled={create.isPending}>
            Create key
          </Button>
          {create.isError && (
            <div className="basis-full">
              <ErrorBox error={create.error} />
            </div>
          )}
        </Card>
      </form>
      {client.keys.length === 0 ? (
        <Card className="text-muted-foreground px-4 py-8 text-center text-sm">No keys yet. A caller needs one to send a review.</Card>
      ) : (
        <Card className="gap-0 overflow-hidden py-0">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Name</TableHead>
                <TableHead>Prefix</TableHead>
                <TableHead>Created</TableHead>
                <TableHead>Expires</TableHead>
                <TableHead>Last used</TableHead>
                <TableHead />
              </TableRow>
            </TableHeader>
            <TableBody>
              {client.keys.map((k) => (
                <KeyRow key={k.id} client={client.name} apiKey={k} />
              ))}
            </TableBody>
          </Table>
        </Card>
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
    <TableRow>
      <TableCell className="font-medium">{k.name}</TableCell>
      <TableCell className="font-mono text-xs">{k.prefix}</TableCell>
      <TableCell className="text-muted-foreground whitespace-nowrap">{utcText(k.createdAt)}</TableCell>
      <TableCell className="text-muted-foreground whitespace-nowrap">{k.expiresAt ? utcText(k.expiresAt) : "never"}</TableCell>
      <TableCell className="text-muted-foreground whitespace-nowrap">{k.lastUsedAt ? utcText(k.lastUsedAt) : "never"}</TableCell>
      <TableCell className="text-right">
        <Button type="button" variant="destructive-outline" size="sm" disabled={revoke.isPending} onClick={confirmRevoke}>
          Revoke
        </Button>
        {revoke.isError && <ErrorBox error={revoke.error} />}
      </TableCell>
    </TableRow>
  );
}
