import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";

import { api, skillBudget, type DefaultsView, type LibrarySkill, type LLMAdminView } from "../api";
import { useWrite } from "../layout/write";
import { ErrorBox, Loading, Switch, bytesText, noFill, secretFill, secretText } from "../ui";

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
  const [capMR, setCapMR] = useState(String(v.rerunLimitPerMR));
  const [capRepo, setCapRepo] = useState(String(v.rerunLimitPerRepo));
  const [skills, setSkills] = useState(v.skills);
  const [problem, setProblem] = useState("");
  const [note, setNote] = useState("");
  const library = useQuery({ queryKey: ["skills"], queryFn: () => api.get<LibrarySkill[]>("/api/skills") });
  const save = useMutation({
    mutationFn: (body: Record<string, unknown>) => write<DefaultsView>("PUT", "/api/admin/defaults", body),
    onMutate: () => setNote(""),
    onSuccess: (saved) => {
      client.setQueryData(["admin-defaults"], saved);
      // The library's "default" marks and the repository pages' default list
      // come from /api/skills.
      client.invalidateQueries({ queryKey: ["skills"] });
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
    // A cap is a whole number from 1 to 1000; empty is the built-in one.
    const caps = [capMR, capRepo].map((c) => c.trim());
    if (caps.some((c) => c !== "" && (!/^\d+$/.test(c) || Number(c) < 1 || Number(c) > 1000))) {
      setProblem("The re-run caps are whole numbers from 1 to 1000.");
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
      rerunLimitPerMR: caps[0] === "" ? null : Number(caps[0]),
      rerunLimitPerRepo: caps[1] === "" ? null : Number(caps[1]),
      // Left out when the library couldn't be read, the stored list is kept.
      ...(library.isSuccess ? { skills: library.data.map((s) => s.name).filter((n) => skills.includes(n)) } : {}),
    });
  };
  return (
    <form className="card" aria-label="Repository defaults" noValidate onSubmit={submit}>
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
      <div className="set">
        <div>
          <div className="name">Re-run cap per MR/PR</div>
          <div className="help">
            The most re-runs (buttons and comments) of one merge request or pull request in a UTC day, 1 to 1000. Pushes
            always run.
          </div>
        </div>
        <div>
          <input
            type="number"
            aria-label="Re-run cap per MR/PR (per day)"
            min={1}
            max={1000}
            step={1}
            value={capMR}
            disabled={save.isPending}
            onChange={(e) => setCapMR(e.target.value)}
          />
        </div>
      </div>
      <div className="set">
        <div>
          <div className="name">Re-run cap per repository</div>
          <div className="help">The most re-runs across a whole repository in a UTC day, 1 to 1000.</div>
        </div>
        <div>
          <input
            type="number"
            aria-label="Re-run cap per repository (per day)"
            min={1}
            max={1000}
            step={1}
            value={capRepo}
            disabled={save.isPending}
            onChange={(e) => setCapRepo(e.target.value)}
          />
        </div>
      </div>
      <div className="set">
        <div>
          <div className="name">Default skills</div>
          <div className="help">
            The library skills a repository reviews when it doesn't choose its own. A review takes at most{" "}
            {bytesText(skillBudget)} of library skill text.
          </div>
        </div>
      </div>
      {library.isPending && <Loading />}
      {library.isError && <ErrorBox error={library.error} />}
      {library.isSuccess && (
        <SkillPicker
          library={library.data}
          // Stored default names the library no longer has (a delete is refused
          // while one is in use, but the list may predate that).
          missing={v.skills.filter((n) => !library.data.some((s) => s.name === n))}
          selected={skills}
          disabled={save.isPending}
          onChange={setSkills}
        />
      )}
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

// SkillPicker picks the default list among the library's skills, with the
// running total of their text against what a review takes.
function SkillPicker({
  library,
  missing,
  selected,
  disabled,
  onChange,
}: {
  library: LibrarySkill[];
  missing: string[];
  selected: string[];
  disabled: boolean;
  onChange: (names: string[]) => void;
}) {
  const total = library.filter((s) => selected.includes(s.name)).reduce((sum, s) => sum + s.size, 0);
  const over = total > skillBudget;
  return (
    <div role="group" aria-label="Default skills">
      {library.length === 0 && <div className="pad meta">The library has no skills yet. Add some under Skills.</div>}
      {library.map((s) => (
        <div className="skill" key={s.name}>
          <Switch
            label={`Default skill ${s.name}`}
            checked={selected.includes(s.name)}
            disabled={disabled}
            onChange={(on) => onChange(on ? [...selected, s.name] : selected.filter((n) => n !== s.name))}
          />
          <b className="grow">{s.name}</b>
          <span className="src">{bytesText(s.size)}</span>
        </div>
      ))}
      {missing.map((n) => (
        <div className="skill" key={`missing/${n}`}>
          <Switch
            label={`Default skill ${n}`}
            checked={selected.includes(n)}
            disabled={disabled}
            onChange={(on) => onChange(on ? [...selected, n] : selected.filter((x) => x !== n))}
          />
          <b className="grow">{n}</b>
          <span className="problem">not in the library</span>
        </div>
      ))}
      {missing.some((n) => selected.includes(n)) && (
        <div className="pad meta">
          {missing.filter((n) => selected.includes(n)).join(", ")} {missing.length === 1 ? "is" : "are"} no longer in the library.
          Saving removes {missing.length === 1 ? "it" : "them"} from the default skills.
        </div>
      )}
      <div className={over ? "pad problem" : "pad meta"}>
        Total {bytesText(total)} of {bytesText(skillBudget)}
      </div>
      {over && <div className="pad problem">A review takes at most that much library skill text; the save will be refused.</div>}
      {selected.length === 0 && (
        <div className="pad meta">
          With no default skills, a repository that doesn't choose its own gets no skills and is not reviewed.
        </div>
      )}
    </div>
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

// providers are the opencode provider IDs offered in the list. Any other
// ID can still be typed under "Other".
const providers: [string, string][] = [
  ["anthropic", "Anthropic (Claude)"],
  ["openai", "OpenAI"],
  ["google", "Google (Gemini)"],
  ["deepseek", "DeepSeek"],
  ["openrouter", "OpenRouter"],
  ["groq", "Groq"],
  ["mistral", "Mistral"],
  ["xai", "xAI (Grok)"],
];

// ModelForm edits the model an operator may change. The key is write-only:
// it is sent when replaced and never shown.
function ModelForm({ v }: { v: LLMAdminView }) {
  const client = useQueryClient();
  const write = useWrite();
  const listed = providers.some(([id]) => id === v.provider);
  const [choice, setChoice] = useState(listed ? v.provider : v.provider ? "other" : "");
  const [otherID, setOtherID] = useState(listed ? "" : v.provider);
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
      provider: (choice === "other" ? otherID : choice).trim(),
      model: model.trim(),
      baseURL: baseURL.trim(),
      ...(askKey && key !== "" ? { apiKey: key } : {}),
    });
  };
  return (
    <form className="card pad stack" aria-label="Model" onSubmit={submit}>
      <h2>Model</h2>
      <label htmlFor="llm-provider">Provider</label>
      <select id="llm-provider" value={choice} onChange={(e) => setChoice(e.target.value)} required>
        <option value="" disabled>
          Choose a provider
        </option>
        {providers.map(([id, name]) => (
          <option key={id} value={id}>
            {name}
          </option>
        ))}
        <option value="other">Other (type its ID)</option>
      </select>
      {choice === "other" && (
        <>
          <label htmlFor="llm-provider-id">Provider ID</label>
          <input
            id="llm-provider-id"
            type="text"
            {...noFill}
            spellCheck={false}
            value={otherID}
            onChange={(e) => setOtherID(e.target.value)}
            required
          />
          <div className="meta">The provider's ID in opencode. With a Base URL, it is any OpenAI-compatible server.</div>
        </>
      )}
      <label htmlFor="llm-model">Model</label>
      <input
        id="llm-model"
        type="text"
        {...noFill}
        spellCheck={false}
        value={model}
        onChange={(e) => setModel(e.target.value)}
        required
      />
      <div className="meta">The model's ID as the provider names it.</div>
      <label htmlFor="llm-base-url">Base URL</label>
      <input
        id="llm-base-url"
        type="text"
        {...noFill}
        spellCheck={false}
        placeholder="the provider's own"
        value={baseURL}
        onChange={(e) => setBaseURL(e.target.value)}
      />
      <div className="meta">Empty uses the provider's own API. Set it for a proxy or an OpenAI-compatible server.</div>
      <div className="name">API key</div>
      <div className="meta">{secretText(v.key)}</div>
      {askKey ? (
        <input
          type="password"
          aria-label="New API key"
          {...secretFill}
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
