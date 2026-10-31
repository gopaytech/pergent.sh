import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";

import { api, type IntegrationView, type InstanceAdminView, type Platform } from "../api";
import { useMe } from "../layout/me";
import { useWrite } from "../layout/write";
import { ErrorBox, Loading, Pill, platformName, secretText } from "../ui";

// Integrations is the instances pergent.sh reviews on. An operator sees
// every one and adds, changes and removes those kept in the dashboard; a
// platform admin sees its own instance, read-only.
export default function Integrations() {
  const me = useMe();
  return me.operator ? <Instances /> : <OwnInstance />;
}

// --- a platform admin's own instance ---

function OwnInstance() {
  const data = useQuery({ queryKey: ["integrations"], queryFn: () => api.get<IntegrationView>("/api/integrations") });
  return (
    <div className="narrow">
      <div className="top">
        <div>
          <h1>Integrations</h1>
          <div className="sub">The instance you signed in with, read-only</div>
        </div>
      </div>
      {data.isPending && <Loading />}
      {data.isError && <ErrorBox error={data.error} />}
      {data.isSuccess && <OwnBody v={data.data} />}
    </div>
  );
}

function OwnBody({ v }: { v: IntegrationView }) {
  const platform = platformName(v.platform);
  const approver = v.approver === "app" ? "approve App" : "bot user token";
  return (
    <>
      <section className="card" aria-label={`${platform} · ${v.instance}`}>
        <h2>
          {platform} · {v.instance}{" "}
          {v.connected ? <Pill tone="ok">Connected</Pill> : <Pill tone="bad">Not connected</Pill>}
        </h2>
        <div className="facts">
          <div>Approver: {approver}</div>
          {v.tokenExpiry && <div className="warn-text">Token expires {v.tokenExpiry.slice(0, 10)}</div>}
        </div>
      </section>
      <section className="card scroll" aria-label="Repositories">
        <h2>Repositories</h2>
        {v.repos.length === 0 ? (
          <div className="row empty-row">No repositories are switched on.</div>
        ) : (
          <table className="table">
            <thead>
              <tr>
                <th>Repository</th>
                <th>{v.platform === "github" ? "Review App" : "Webhook and deploy token"}</th>
                <th>Approver</th>
              </tr>
            </thead>
            <tbody>
              {v.repos.map((r) => (
                <tr key={r.project}>
                  <td className="mr">{r.project}</td>
                  <td>{r.installed ? <Pill tone="ok">Installed</Pill> : <Pill tone="bad">Not installed</Pill>}</td>
                  <td>{r.approverReachable ? <Pill tone="ok">Can approve</Pill> : <Pill tone="bad">Can't approve</Pill>}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </section>
    </>
  );
}

// --- an operator's instances ---

// secretOrder is the order a card lists the secrets an instance takes, and
// secretLabels what each is called.
const secretOrder = [
  "review_token",
  "approve_token",
  "review_app_key",
  "approve_app_key",
  "webhook_secret",
  "oauth_client_secret",
];
const secretLabels: Record<string, string> = {
  review_token: "Review token",
  approve_token: "Approve token",
  review_app_key: "Review App private key",
  approve_app_key: "Approve App private key",
  webhook_secret: "Webhook secret",
  oauth_client_secret: "OAuth client secret",
};
const isKey = (field: string) => field.endsWith("_app_key");

// normalizePEM puts a pasted PEM key back in lines. A password input is one
// line: the browser turns the key's line breaks into spaces or drops them,
// and the server, which wants the PEM's own lines, would refuse it.
function normalizePEM(text: string): string {
  const m = /^\s*(-----BEGIN [A-Z0-9 ]+-----)([\s\S]*?)(-----END [A-Z0-9 ]+-----)\s*$/.exec(text);
  if (!m) return text;
  const lines = m[2].replace(/\s+/g, "").match(/.{1,64}/g) ?? [];
  return `${m[1]}\n${lines.join("\n")}\n${m[3]}\n`;
}

// secretValue is what is sent for a secret typed into field.
const secretValue = (field: string, text: string) => (isKey(field) ? normalizePEM(text) : text);

// removingPoll is how often the list is read while an instance is being
// removed, in ms: the server takes the webhooks off and deletes it within a
// minute or so.
const removingPoll = 3000;

function Instances() {
  const [adding, setAdding] = useState<Platform | null>(null);
  const list = useQuery({
    queryKey: ["admin-instances"],
    queryFn: () => api.get<InstanceAdminView[]>("/api/admin/instances"),
    refetchInterval: (q) => (q.state.data?.some((i) => i.removing) ? removingPoll : false),
  });
  return (
    <div className="narrow">
      <div className="top">
        <div>
          <h1>Integrations</h1>
          <div className="sub">
            The GitLab and GitHub instances pergent.sh reviews on. Secrets are write-only: once saved they are never shown.
          </div>
        </div>
        <div className="actions-row">
          <button type="button" className="btn" onClick={() => setAdding(adding === "gitlab" ? null : "gitlab")}>
            Add GitLab instance
          </button>
          <button type="button" className="btn" onClick={() => setAdding(adding === "github" ? null : "github")}>
            Add GitHub instance
          </button>
        </div>
      </div>
      {adding && <AddInstance key={adding} platform={adding} onClose={() => setAdding(null)} />}
      {list.isPending && <Loading />}
      {list.isError && <ErrorBox error={list.error} />}
      {list.isSuccess &&
        (list.data.length === 0 ? (
          <div className="card empty">No instance yet. Add a GitLab or GitHub instance to start.</div>
        ) : (
          list.data.map((inst) => <InstanceCard key={inst.name} inst={inst} />)
        ))}
    </div>
  );
}

// useChange returns the mutation that changes an instance of the dashboard:
// body is {oauthClientID} and/or {secrets}.
function useChange(name: string, onDone: () => void) {
  const client = useQueryClient();
  const write = useWrite();
  return useMutation({
    mutationFn: (body: { oauthClientID?: string; secrets?: Record<string, string> }) =>
      write<InstanceAdminView>("PUT", `/api/admin/instances/${encodeURIComponent(name)}`, body),
    // The closure holds a secret: drop it with the input, not after 5 minutes.
    gcTime: 0,
    onSuccess: () => {
      onDone();
      return client.invalidateQueries({ queryKey: ["admin-instances"] });
    },
  });
}

function InstanceCard({ inst }: { inst: InstanceAdminView }) {
  const client = useQueryClient();
  const write = useWrite();
  const config = inst.source === "config";
  const locked = config || inst.removing;
  const remove = useMutation({
    mutationFn: () => write<{ status: string }>("DELETE", `/api/admin/instances/${encodeURIComponent(inst.name)}`),
    onSuccess: () => client.invalidateQueries({ queryKey: ["admin-instances"] }),
  });
  const confirmRemove = () => {
    if (
      window.confirm(
        `Remove ${inst.name}? Its repositories are switched off, and its accounts, sessions and secrets are deleted.`,
      )
    ) {
      remove.mutate();
    }
  };
  const platform = platformName(inst.platform);
  const fields = secretOrder.filter((f) => f in inst.secrets);
  return (
    <section className="card" aria-label={`${platform} · ${inst.name}`}>
      <h2>
        <span>
          {platform} · {inst.name}
        </span>
        <span className="actions-row">
          {config && <span className="badge cfg">config</span>}
          {inst.removing && <Pill tone="warn">removing</Pill>}
        </span>
      </h2>
      <div className="facts">
        {inst.platform === "gitlab" ? (
          <div>Address: {inst.url}</div>
        ) : (
          <>
            <div>API address: {inst.apiURL}</div>
            <div>Review App ID: {inst.reviewAppID}</div>
            {inst.approver === "app" && <div>Approve App ID: {inst.approveAppID}</div>}
          </>
        )}
        <div>Approver: {inst.approver === "app" ? "approve App" : "bot user token"}</div>
        {config && <div className="meta">This instance is managed by the config file; change it there.</div>}
      </div>
      <OAuthClientRow inst={inst} locked={locked} />
      {fields.map((f) => (
        <SecretRow
          key={f}
          inst={inst}
          field={f}
          // pergent.sh generates a GitLab webhook secret; it can't be replaced.
          locked={locked || (inst.platform === "gitlab" && f === "webhook_secret")}
        />
      ))}
      {remove.isError && <ErrorBox error={remove.error} />}
      {!locked && (
        <div className="foot">
          <button type="button" className="btn small" disabled={remove.isPending} onClick={confirmRemove}>
            Remove
          </button>
        </div>
      )}
    </section>
  );
}

function OAuthClientRow({ inst, locked }: { inst: InstanceAdminView; locked: boolean }) {
  const [open, setOpen] = useState(false);
  const [value, setValue] = useState("");
  const change = useChange(inst.name, () => setOpen(false));
  const submit = (e: FormEvent) => {
    e.preventDefault();
    change.mutate({ oauthClientID: value.trim() });
  };
  return (
    <div className="row wrap" role="group" aria-label="OAuth client ID">
      <div className="grow">
        <div className="name">OAuth client ID</div>
        <div className="meta">{inst.oauthClientID || "not set"}</div>
      </div>
      {!locked && !open && (
        <button
          type="button"
          className="btn small"
          onClick={() => {
            change.reset();
            setValue(inst.oauthClientID);
            setOpen(true);
          }}
        >
          Change
        </button>
      )}
      {open && (
        <form className="inline-form" onSubmit={submit}>
          <input
            type="text"
            aria-label="New OAuth client ID"
            autoComplete="off"
            spellCheck={false}
            value={value}
            onChange={(e) => setValue(e.target.value)}
          />
          <button type="submit" className="btn small primary" disabled={change.isPending}>
            Save
          </button>
          <button type="button" className="btn small" onClick={() => setOpen(false)}>
            Cancel
          </button>
        </form>
      )}
      {change.isError && <ErrorBox error={change.error} />}
    </div>
  );
}

function SecretRow({ inst, field, locked }: { inst: InstanceAdminView; field: string; locked: boolean }) {
  const label = secretLabels[field] ?? field;
  const [open, setOpen] = useState(false);
  const [value, setValue] = useState("");
  const close = () => {
    setOpen(false);
    setValue("");
  };
  const change = useChange(inst.name, close);
  const submit = (e: FormEvent) => {
    e.preventDefault();
    change.mutate({ secrets: { [field]: secretValue(field, value) } });
  };
  return (
    <div className="row wrap" role="group" aria-label={label}>
      <div className="grow">
        <div className="name">{label}</div>
        <div className="meta">{secretText(inst.secrets[field])}</div>
      </div>
      {!locked && !open && (
        <button
          type="button"
          className="btn small"
          onClick={() => {
            change.reset();
            setOpen(true);
          }}
        >
          Replace
        </button>
      )}
      {open && (
        <form className="inline-form" onSubmit={submit}>
          <input
            type="password"
            aria-label={`New ${label}`}
            autoComplete="off"
            spellCheck={false}
            value={value}
            onChange={(e) => setValue(e.target.value)}
            required
          />
          <button type="submit" className="btn small primary" disabled={change.isPending}>
            Save
          </button>
          <button type="button" className="btn small" onClick={close}>
            Cancel
          </button>
        </form>
      )}
      {change.isError && <ErrorBox error={change.error} />}
    </div>
  );
}

// --- adding an instance ---

function Field({
  d,
  set,
  name,
  label,
  secret,
  type = "text",
  help,
  required,
}: {
  d: Record<string, string>;
  set: (k: string, v: string) => void;
  name: string;
  label: string;
  secret?: boolean;
  type?: "text" | "number";
  help?: string;
  required?: boolean;
}) {
  const id = `add-${name}`;
  return (
    <>
      <label htmlFor={id}>{label}</label>
      <input
        id={id}
        // A secret is typed into a password input that never prefills.
        type={secret ? "password" : type}
        autoComplete="off"
        autoCapitalize="none"
        spellCheck={false}
        value={d[name] ?? ""}
        onChange={(e) => set(name, e.target.value)}
        required={required}
      />
      {help && <div className="meta">{help}</div>}
    </>
  );
}

function AddInstance({ platform, onClose }: { platform: Platform; onClose: () => void }) {
  const client = useQueryClient();
  const write = useWrite();
  const [d, setD] = useState<Record<string, string>>({});
  const set = (k: string, v: string) => setD((old) => ({ ...old, [k]: v }));
  const [problem, setProblem] = useState("");
  const add = useMutation({
    mutationFn: (body: unknown) => write<InstanceAdminView>("POST", "/api/admin/instances", body),
    // The closure holds the secrets: drop it with the form.
    gcTime: 0,
    onSuccess: () => {
      onClose();
      return client.invalidateQueries({ queryKey: ["admin-instances"] });
    },
  });
  const github = platform === "github";
  // A blank Approve App ID asks for the App's key; 0 means a bot user's
  // token approves.
  const approveWithApp = github && (d.approveAppID ?? "").trim() !== "0";
  const v = (k: string) => (d[k] ?? "").trim();

  const submit = (e: FormEvent) => {
    e.preventDefault();
    // The webhook secret of a GitLab instance is generated by the server.
    const typed: Record<string, string> = github
      ? {
          review_app_key: d.review_app_key ?? "",
          ...(approveWithApp ? { approve_app_key: d.approve_app_key ?? "" } : { approve_token: d.approve_token ?? "" }),
          webhook_secret: d.webhook_secret ?? "",
          oauth_client_secret: d.oauth_client_secret ?? "",
        }
      : {
          review_token: d.review_token ?? "",
          approve_token: d.approve_token ?? "",
          oauth_client_secret: d.oauth_client_secret ?? "",
        };
    const secrets: Record<string, string> = {};
    for (const [field, text] of Object.entries(typed)) {
      if (text.trim() !== "") secrets[field] = secretValue(field, text);
    }
    if (github) {
      const reviewAppID = Number(v("reviewAppID"));
      const approveAppID = Number(v("approveAppID"));
      if (v("approveAppID") === "" || !Number.isInteger(reviewAppID) || !Number.isInteger(approveAppID)) {
        setProblem("The App IDs are whole numbers. Give the Approve App ID 0 if a bot user's token approves.");
        return;
      }
      setProblem("");
      add.mutate({
        name: v("name"),
        platform,
        apiURL: v("apiURL"),
        reviewAppID,
        approveAppID,
        oauthClientID: v("oauthClientID"),
        secrets,
      });
      return;
    }
    setProblem("");
    add.mutate({ name: v("name"), platform, url: v("url"), oauthClientID: v("oauthClientID"), secrets });
  };

  const title = `Add ${platformName(platform)} instance`;
  return (
    <form className="card pad stack" aria-label={title} onSubmit={submit}>
      <h2>{title}</h2>
      <Field
        d={d}
        set={set}
        name="name"
        label="Name"
        required
        help={'1 to 63 lowercase letters, digits or "-". It can\'t change.'}
      />
      {github ? (
        <>
          <Field d={d} set={set} name="apiURL" label="API address" required help="The https address of the GitHub API." />
          <Field d={d} set={set} name="reviewAppID" label="Review App ID" type="number" required />
          <Field
            d={d}
            set={set}
            name="approveAppID"
            label="Approve App ID"
            type="number"
            help="0 if a bot user's token approves."
          />
          <Field d={d} set={set} name="review_app_key" label="Review App private key" secret required />
          {approveWithApp ? (
            <Field d={d} set={set} name="approve_app_key" label="Approve App private key" secret required />
          ) : (
            <Field d={d} set={set} name="approve_token" label="Approve token" secret required />
          )}
          <Field d={d} set={set} name="webhook_secret" label="Webhook secret" secret required />
        </>
      ) : (
        <>
          <Field d={d} set={set} name="url" label="Address" required help="The https address of the GitLab server." />
          <Field d={d} set={set} name="review_token" label="Review token" secret required />
          <Field d={d} set={set} name="approve_token" label="Approve token" secret required />
        </>
      )}
      <Field
        d={d}
        set={set}
        name="oauthClientID"
        label="OAuth client ID"
        help="Optional: lets people sign in to the dashboard with this instance. Give the client secret too."
      />
      <Field d={d} set={set} name="oauth_client_secret" label="OAuth client secret" secret />
      {problem && (
        <div className="error" role="alert">
          {problem}
        </div>
      )}
      {add.isError && <ErrorBox error={add.error} />}
      <div className="actions-row">
        <button type="submit" className="btn primary" disabled={add.isPending}>
          {add.isPending ? "Checking…" : "Add"}
        </button>
        <button type="button" className="btn" onClick={onClose}>
          Cancel
        </button>
      </div>
    </form>
  );
}
