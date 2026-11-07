import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useState, type FormEvent } from "react";

import { api, type Approver, type InstanceAdminView, type Platform } from "../api";
import { useWrite } from "../layout/write";
import { ErrorBox, Loading, Pill, noFill, platformName, secretFill, secretText } from "../ui";

// Integrations is the instances pergent.sh reviews on, for admins: they see
// every one and add, change and remove those kept in the dashboard.
export default function Integrations() {
  return <Instances />;
}

// approverWords says who approves, for a card.
function approverWords(approver: Approver): string {
  switch (approver) {
    case "review_app":
      return "Approves with the review App";
    case "app":
      return "Approves with the approve App";
    default:
      return "Approves with a bot user's token";
  }
}

// secretOrder is the order a card lists the secrets an instance takes, and
// secretLabels what each is called.
const secretOrder = [
  "review_token",
  "approve_token",
  "review_app_key",
  "approve_app_key",
  "webhook_secret",
];
const secretLabels: Record<string, string> = {
  review_token: "Review token",
  approve_token: "Approve token",
  review_app_key: "Review App private key",
  approve_app_key: "Approve App private key",
  webhook_secret: "Webhook secret",
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
// body is {secrets}.
function useChange(name: string, onDone: () => void) {
  const client = useQueryClient();
  const write = useWrite();
  return useMutation({
    mutationFn: (body: { secrets: Record<string, string> }) =>
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
        `Remove ${inst.name}? Its repositories are switched off, and its secrets are deleted.`,
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
        <WebhookURL inst={inst} />
        <div>{approverWords(inst.approver)}</div>
        {config && <div className="meta">This instance is managed by the config file; change it there.</div>}
      </div>
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

function WebhookURL({ inst }: { inst: InstanceAdminView }) {
  const [copied, setCopied] = useState(false);
  useEffect(() => {
    if (!copied) {
      return;
    }
    const timer = setTimeout(() => setCopied(false), 1500);
    return () => clearTimeout(timer);
  }, [copied]);
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(inst.webhookURL);
      setCopied(true);
    } catch {
      // No clipboard here: the URL is on screen to select.
    }
  };
  return (
    <div className="webhook-url">
      Webhook URL: <code>{inst.webhookURL}</code>{" "}
      <button type="button" className="btn small" aria-label={copied ? "Copied" : "Copy webhook URL"} onClick={copy}>
        {copied ? "Copied" : "Copy"}
      </button>
      <div className="meta">
        {inst.platform === "gitlab" ? "pergent.sh sets this on each project." : "Set this on the review App."}
      </div>
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
            {...secretFill}
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
        {...(secret ? secretFill : noFill)}
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
  // Who approves decides the Approve fields: only the chosen kind's are
  // shown and sent.
  const approveWith: Approver = (d.approveWith as Approver | undefined) ?? "review_app";
  const v = (k: string) => (d[k] ?? "").trim();

  const submit = (e: FormEvent) => {
    e.preventDefault();
    // The webhook secret of a GitLab instance is generated by the server.
    const typed: Record<string, string> = github
      ? {
          review_app_key: d.review_app_key ?? "",
          ...(approveWith === "app" ? { approve_app_key: d.approve_app_key ?? "" } : {}),
          ...(approveWith === "token" ? { approve_token: d.approve_token ?? "" } : {}),
          webhook_secret: d.webhook_secret ?? "",
        }
      : {
          review_token: d.review_token ?? "",
          approve_token: d.approve_token ?? "",
        };
    const secrets: Record<string, string> = {};
    for (const [field, text] of Object.entries(typed)) {
      if (text.trim() !== "") secrets[field] = secretValue(field, text);
    }
    if (github) {
      const reviewAppID = Number(v("reviewAppID"));
      // The server reads the approve App ID: the review App's own is the
      // review App approving, 0 a bot user's token.
      let approveAppID = reviewAppID;
      if (approveWith === "token") approveAppID = 0;
      if (approveWith === "app") approveAppID = Number(v("approveAppID"));
      if (v("reviewAppID") === "" || !Number.isInteger(reviewAppID) || !Number.isInteger(approveAppID)) {
        setProblem("The App IDs are whole numbers.");
        return;
      }
      if (approveWith === "app" && (v("approveAppID") === "" || approveAppID <= 0)) {
        setProblem("The approve App ID is a whole number above 0.");
        return;
      }
      if (approveWith === "app" && approveAppID === reviewAppID) {
        setProblem("The approve App must be a different App from the review App. To use one App, approve with the review App.");
        return;
      }
      setProblem("");
      add.mutate({
        name: v("name"),
        platform,
        apiURL: v("apiURL"),
        reviewAppID,
        approveAppID,
        secrets,
      });
      return;
    }
    setProblem("");
    add.mutate({ name: v("name"), platform, url: v("url"), secrets });
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
          <Field d={d} set={set} name="review_app_key" label="Review App private key" secret required />
          <label htmlFor="add-approveWith">Approve with</label>
          <select id="add-approveWith" value={approveWith} onChange={(e) => set("approveWith", e.target.value)}>
            <option value="review_app">The review App (one App)</option>
            <option value="app">A separate approve App</option>
            <option value="token">A bot user's token</option>
          </select>
          <div className="meta">
            One App is the simplest: it then needs the Pull requests: write permission. A separate App or a bot user keeps the
            identity that writes the review from being the one that approves it.
          </div>
          {approveWith === "app" && (
            <>
              <Field d={d} set={set} name="approveAppID" label="Approve App ID" type="number" required />
              <Field d={d} set={set} name="approve_app_key" label="Approve App private key" secret required />
            </>
          )}
          {approveWith === "token" && (
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
