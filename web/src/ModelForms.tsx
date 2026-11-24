// The forms that add and change a model entry, for the admin's list of
// models and a team's own. Keys are write-only: a key is sent when typed,
// the input is emptied after, and no key is ever shown.
import { useMutation } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";

import type { SecretView } from "./api";
import { useWrite } from "./layout/write";
import { ErrorBox, Switch, noFill, secretFill, secretText } from "./ui";

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

const nameHelp = "Up to 64 letters, digits, spaces and . _ : / + -. After a backup model reviews a change, the comment names it.";
const baseURLHelp = "Empty uses the provider's own API. Set it for a proxy or an OpenAI-compatible server.";

// AddModelForm adds an entry with a POST to path: its name, provider, model,
// base URL and key, and with teamsMayUse, the admins' switch that lets team
// chains use it. onAdded gets the new entry's name. With onCancel, the form
// sits inside the caller's Models card, opened by its Add model button,
// with a Cancel beside Add model; without, it is a card of its own.
export function AddModelForm<T extends { name: string }>({
  path,
  teamsMayUse,
  onAdded,
  onCancel,
}: {
  path: string;
  teamsMayUse?: boolean;
  onAdded: (added: T) => void;
  onCancel?: () => void;
}) {
  const inCard = onCancel !== undefined;
  const write = useWrite();
  const [name, setName] = useState("");
  const [choice, setChoice] = useState("");
  const [otherID, setOtherID] = useState("");
  const [model, setModel] = useState("");
  const [baseURL, setBaseURL] = useState("");
  const [key, setKey] = useState("");
  const [forTeams, setForTeams] = useState(false);
  const [note, setNote] = useState("");
  const add = useMutation({
    mutationFn: (body: Record<string, string | boolean>) => write<T>("POST", path, body),
    // The closure holds the key: drop it with the input, not after 5 minutes.
    gcTime: 0,
    onMutate: () => setNote(""),
    onSuccess: (added) => {
      setName("");
      setChoice("");
      setOtherID("");
      setModel("");
      setBaseURL("");
      setKey("");
      setForTeams(false);
      setNote(`Added ${added.name}.`);
      onAdded(added);
    },
  });
  const submit = (e: FormEvent) => {
    e.preventDefault();
    add.mutate({
      name: name.trim(),
      provider: (choice === "other" ? otherID : choice).trim(),
      model: model.trim(),
      baseURL: baseURL.trim(),
      apiKey: key,
      ...(teamsMayUse ? { teamsMayUse: forTeams } : {}),
    });
  };
  return (
    <form className={inCard ? "add-model" : "card"} aria-label="Add a model" onSubmit={submit}>
      {inCard ? <h3>Add a model</h3> : <h2>Add a model</h2>}
      <div className="pad stack">
        <label htmlFor="llm-name">Name</label>
        <input
          id="llm-name"
          type="text"
          {...noFill}
          spellCheck={false}
          maxLength={64}
          value={name}
          onChange={(e) => setName(e.target.value)}
          required
        />
        <div className="meta">{nameHelp}</div>
        <label htmlFor="llm-provider">Provider</label>
        <select id="llm-provider" value={choice} onChange={(e) => setChoice(e.target.value)} required>
          <option value="" disabled>
            Choose a provider
          </option>
          {providers.map(([id, label]) => (
            <option key={id} value={id}>
              {label}
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
        <div className="meta">{baseURLHelp}</div>
        <label htmlFor="llm-api-key">API key</label>
        <input
          id="llm-api-key"
          type="password"
          {...secretFill}
          spellCheck={false}
          value={key}
          onChange={(e) => setKey(e.target.value)}
          required
        />
        {teamsMayUse && (
          <label className="link-block">
            <Switch label="Teams may use it" checked={forTeams} onChange={setForTeams} />
            <span>Teams may use it in their chains. Its key then pays for their reviews.</span>
          </label>
        )}
        {add.isError && <ErrorBox error={add.error} />}
      </div>
      <div className="foot">
        {note && (
          <span className="meta grow" role="status">
            {note}
          </span>
        )}
        {inCard && (
          <button type="button" className="btn" disabled={add.isPending} onClick={onCancel}>
            Cancel
          </button>
        )}
        <button type="submit" className="btn primary" disabled={add.isPending}>
          Add model
        </button>
      </div>
    </form>
  );
}

// EditableModel is what EditModelForm changes of an entry.
export type EditableModel = { id: string; name: string; baseURL: string; key: SecretView };

// EditModelForm changes an entry's name, its base URL and, when typed, its
// key, with a PUT to path. The provider and model never change: another one
// is a new entry.
export function EditModelForm({
  m,
  path,
  onDone,
  onSaved,
}: {
  m: EditableModel;
  path: string;
  onDone: () => void;
  onSaved: () => void;
}) {
  const write = useWrite();
  const [name, setName] = useState(m.name);
  const [baseURL, setBaseURL] = useState(m.baseURL);
  const [key, setKey] = useState("");
  // With a key stored, its input waits for Replace; with none, it is offered.
  const [replacing, setReplacing] = useState(false);
  const askKey = !m.key.set || replacing;
  const save = useMutation({
    mutationFn: (body: Record<string, string>) => write<unknown>("PUT", path, body),
    // The closure holds the key: drop it with the input, not after 5 minutes.
    gcTime: 0,
    onSuccess: () => {
      setKey("");
      onSaved();
      onDone();
    },
  });
  const submit = (e: FormEvent) => {
    e.preventDefault();
    // An empty base URL clears it; a key goes only when one was typed.
    save.mutate({ name: name.trim(), baseURL: baseURL.trim(), ...(askKey && key !== "" ? { apiKey: key } : {}) });
  };
  const id = `model-${m.id}`;
  return (
    <form className="stack" aria-label={`Edit ${m.name}`} onSubmit={submit}>
      <label htmlFor={`${id}-name`}>Name</label>
      <input
        id={`${id}-name`}
        type="text"
        {...noFill}
        spellCheck={false}
        maxLength={64}
        value={name}
        onChange={(e) => setName(e.target.value)}
        required
      />
      <label htmlFor={`${id}-base-url`}>Base URL</label>
      <input
        id={`${id}-base-url`}
        type="text"
        {...noFill}
        spellCheck={false}
        placeholder="the provider's own"
        value={baseURL}
        onChange={(e) => setBaseURL(e.target.value)}
      />
      <div className="meta">{baseURLHelp}</div>
      <div className="name">API key</div>
      <div className="meta">{secretText(m.key)}</div>
      {askKey ? (
        <input
          type="password"
          aria-label="New API key"
          {...secretFill}
          spellCheck={false}
          value={key}
          onChange={(e) => setKey(e.target.value)}
        />
      ) : (
        <div>
          <button type="button" className="btn small" onClick={() => setReplacing(true)}>
            Replace
          </button>
        </div>
      )}
      {save.isError && <ErrorBox error={save.error} />}
      <div className="actions-row end">
        <button type="button" className="btn small" disabled={save.isPending} onClick={onDone}>
          Cancel
        </button>
        <button type="submit" className="btn primary small" disabled={save.isPending}>
          Save
        </button>
      </div>
    </form>
  );
}
