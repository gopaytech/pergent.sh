import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";

import { api, type DefaultsView, type LLMAdminView } from "../api";
import { useWrite } from "../layout/write";
import { ErrorBox, Loading, Switch, secretText } from "../ui";

// AdminSettings holds what an admin sets for the whole of pergent.sh: the
// defaults a repository switched on in the dashboard gets, and the model
// reviews use.
export default function AdminSettings() {
  const defaults = useQuery({ queryKey: ["admin-defaults"], queryFn: () => api.get<DefaultsView>("/api/admin/defaults") });
  const llm = useQuery({ queryKey: ["admin-llm"], queryFn: () => api.get<LLMAdminView>("/api/admin/llm") });
  return (
    <div className="narrow">
      <div className="top">
        <div>
          <h1>Settings</h1>
          <div className="sub">What a repository starts with, and the model reviews use.</div>
        </div>
      </div>
      {defaults.isPending && <Loading />}
      {defaults.isError && <ErrorBox error={defaults.error} />}
      {defaults.isSuccess && <Defaults v={defaults.data} />}
      {llm.isPending && <Loading />}
      {llm.isError && <ErrorBox error={llm.error} />}
      {llm.isSuccess && (llm.data.editable ? <ModelForm v={llm.data} /> : <ModelView v={llm.data} />)}
    </div>
  );
}

// --- the defaults ---

function Defaults({ v }: { v: DefaultsView }) {
  const client = useQueryClient();
  const write = useWrite();
  const [approve, setApprove] = useState(v.approve);
  const [reviewDrafts, setReviewDrafts] = useState(v.reviewDrafts);
  const [failureStatus, setFailureStatus] = useState(v.failureStatus);
  const [timeout, setTimeoutText] = useState(String(v.jobTimeoutSeconds));
  const [memory, setMemory] = useState(v.jobMemory);
  const [problem, setProblem] = useState("");
  const [note, setNote] = useState("");
  const save = useMutation({
    mutationFn: (body: Record<string, unknown>) => write<DefaultsView>("PUT", "/api/admin/defaults", body),
    onMutate: () => setNote(""),
    onSuccess: (saved) => {
      client.setQueryData(["admin-defaults"], saved);
      setNote("Defaults saved.");
    },
  });
  const submit = (e: FormEvent) => {
    e.preventDefault();
    const t = timeout.trim();
    if (!/^\d+$/.test(t)) {
      setProblem("The timeout is a whole number of seconds, from 60 to 7200.");
      return;
    }
    setProblem("");
    // The PUT replaces every default; null is the built-in one.
    save.mutate({
      approve,
      reviewDrafts,
      failureStatus,
      jobTimeoutSeconds: Number(t),
      jobMemory: memory.trim() === "" ? null : memory.trim(),
    });
  };
  return (
    <form className="card" aria-label="Repository defaults" onSubmit={submit}>
      <h2>Repository defaults</h2>
      <div className="pad meta">
        What a repository you switch on in the dashboard starts with. A repository can override each in its own settings;
        those listed in the config file keep the config's.
      </div>
      <div className="set">
        <div>
          <div className="name">Approve clean reviews</div>
          <div className="help">When off, pergent still reviews and comments but never approves.</div>
        </div>
        <div>
          <Switch label="Approve clean reviews" checked={approve} disabled={save.isPending} onChange={setApprove} />
        </div>
      </div>
      <div className="set">
        <div>
          <div className="name">Review drafts</div>
          <div className="help">When off, a draft isn't reviewed until it is marked ready.</div>
        </div>
        <div>
          <Switch label="Review drafts" checked={reviewDrafts} disabled={save.isPending} onChange={setReviewDrafts} />
        </div>
      </div>
      <div className="set">
        <div>
          <div className="name">Status when a review fails</div>
          <div className="help">"neutral" won't block merging when the LLM provider is down.</div>
        </div>
        <div>
          <select
            aria-label="Status when a review fails"
            value={failureStatus}
            disabled={save.isPending}
            onChange={(e) => setFailureStatus(e.target.value)}
          >
            <option value="failed">failed (blocks merge)</option>
            <option value="neutral">neutral</option>
          </select>
        </div>
      </div>
      <div className="set">
        <div>
          <div className="name">Timeout</div>
          <div className="help">How long one review may run, in seconds (60 to 7200).</div>
        </div>
        <div>
          <input
            type="number"
            aria-label="Timeout (seconds)"
            min={60}
            max={7200}
            step={1}
            value={timeout}
            disabled={save.isPending}
            onChange={(e) => setTimeoutText(e.target.value)}
          />
        </div>
      </div>
      <div className="set">
        <div>
          <div className="name">Memory limit</div>
          <div className="help">For example 6Gi (256Mi to 32Gi). Empty means none.</div>
        </div>
        <div>
          <input
            type="text"
            aria-label="Memory limit"
            placeholder="none"
            value={memory}
            disabled={save.isPending}
            onChange={(e) => setMemory(e.target.value)}
          />
        </div>
      </div>
      {save.isError && <ErrorBox error={save.error} />}
      <div className="foot">
        {problem && <span className="meta problem">{problem}</span>}
        {note && (
          <span className="meta grow" role="status">
            {note}
          </span>
        )}
        <button type="submit" className="btn primary" disabled={save.isPending}>
          Save defaults
        </button>
      </div>
    </form>
  );
}

// --- the model ---

function Item({ k, v }: { k: string; v: string }) {
  return (
    <div className="kv">
      <dt>{k}</dt>
      <dd>{v}</dd>
    </div>
  );
}

// ModelView shows the model to an account that can't change it. The key is
// only ever whether it is set.
function ModelView({ v }: { v: LLMAdminView }) {
  return (
    <section className="card" aria-label="Model">
      <h2>
        <span>Model</span>
        {v.source === "config" && <span className="badge cfg">from the config file</span>}
      </h2>
      <dl className="kvs">
        <Item k="Provider" v={v.provider || "not set"} />
        <Item k="Model" v={v.model || "not set"} />
        {v.baseURL && <Item k="Base URL" v={v.baseURL} />}
        <Item k="Key" v={secretText(v.key)} />
      </dl>
      {v.source !== "config" && <div className="foot meta">Only a password admin can change the model.</div>}
    </section>
  );
}

// ModelForm edits the model an operator may change. The key is write-only:
// it is sent when replaced and never shown.
function ModelForm({ v }: { v: LLMAdminView }) {
  const client = useQueryClient();
  const write = useWrite();
  const [provider, setProvider] = useState(v.provider);
  const [model, setModel] = useState(v.model);
  const [baseURL, setBaseURL] = useState(v.baseURL);
  const [key, setKey] = useState("");
  // With a key stored, its input waits for Replace; with none, it is needed.
  const [replacing, setReplacing] = useState(false);
  const [note, setNote] = useState("");
  const askKey = !v.key.set || replacing;
  const save = useMutation({
    mutationFn: (body: Record<string, string>) => write<LLMAdminView>("PUT", "/api/admin/llm", body),
    // The closure holds the key: drop it with the input, not after 5 minutes.
    gcTime: 0,
    onMutate: () => setNote(""),
    onSuccess: (saved) => {
      setKey("");
      setReplacing(false);
      setNote("Model saved.");
      client.setQueryData(["admin-llm"], saved);
    },
  });
  const submit = (e: FormEvent) => {
    e.preventDefault();
    save.mutate({
      provider: provider.trim(),
      model: model.trim(),
      baseURL: baseURL.trim(),
      ...(askKey && key !== "" ? { apiKey: key } : {}),
    });
  };
  return (
    <form className="card pad stack" aria-label="Model" onSubmit={submit}>
      <h2>Model</h2>
      <label htmlFor="llm-provider">Provider</label>
      <input
        id="llm-provider"
        type="text"
        autoComplete="off"
        spellCheck={false}
        value={provider}
        onChange={(e) => setProvider(e.target.value)}
        required
      />
      <label htmlFor="llm-model">Model</label>
      <input
        id="llm-model"
        type="text"
        autoComplete="off"
        spellCheck={false}
        value={model}
        onChange={(e) => setModel(e.target.value)}
        required
      />
      <label htmlFor="llm-base-url">Base URL</label>
      <input
        id="llm-base-url"
        type="text"
        autoComplete="off"
        spellCheck={false}
        placeholder="the provider's own"
        value={baseURL}
        onChange={(e) => setBaseURL(e.target.value)}
      />
      <div className="name">API key</div>
      <div className="meta">{secretText(v.key)}</div>
      {askKey ? (
        <input
          type="password"
          aria-label="New API key"
          autoComplete="off"
          spellCheck={false}
          value={key}
          onChange={(e) => setKey(e.target.value)}
          required={!v.key.set}
        />
      ) : (
        <div>
          <button type="button" className="btn small" onClick={() => setReplacing(true)}>
            Replace
          </button>
        </div>
      )}
      {save.isError && <ErrorBox error={save.error} />}
      {note && (
        <div className="note" role="status">
          {note}
        </div>
      )}
      <div className="actions-row">
        <button type="submit" className="btn primary" disabled={save.isPending}>
          Save model
        </button>
      </div>
    </form>
  );
}
