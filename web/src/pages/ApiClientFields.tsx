import { useQuery } from "@tanstack/react-query";
import { useId } from "react";

import { api, type LibrarySkill, type ModelsAdminView } from "../api";
import { ErrorBox, Loading, Switch } from "../ui";

// ClientFieldsValue is what the new-client form and the settings tab share:
// the skills, the model ("" is the default model) and the callback URL.
export type ClientFieldsValue = { skills: string[]; modelId: string; callbackUrl: string };

// inLibraryOrder puts the skills that are in the library in its order, then
// the rest as they were, so a body does not depend on the order of clicks.
export function inLibraryOrder(skills: string[], library: string[]): string[] {
  return [...library.filter((n) => skills.includes(n)), ...skills.filter((n) => !library.includes(n))];
}

// ClientFields is the skill picker, the model choice and the callback URL of
// an API client. A client's skills come from the library, as a repository's;
// one the library no longer has is shown, so saving does not drop it unseen.
export function ClientFields({ value, onChange }: { value: ClientFieldsValue; onChange: (v: ClientFieldsValue) => void }) {
  const id = useId();
  const skills = useQuery({ queryKey: ["skills"], queryFn: () => api.get<LibrarySkill[]>("/api/skills") });
  const models = useQuery({ queryKey: ["admin-models"], queryFn: () => api.get<ModelsAdminView>("/api/admin/models") });
  const library = skills.data?.map((s) => s.name) ?? [];
  const names = [...library, ...value.skills.filter((n) => !library.includes(n))];
  const choices = models.data?.models ?? [];
  const label = (m: { provider: string; model: string }) => `${m.provider} / ${m.model}`;
  const def = choices.find((m) => m.default);
  return (
    <>
      <h3 className="group">Skills</h3>
      {skills.isPending && <Loading />}
      {skills.isError && <ErrorBox error={skills.error} />}
      {skills.isSuccess && names.length === 0 && (
        <div className="meta">The library has no skills yet. A client with no skills can't review.</div>
      )}
      {names.map((n) => (
        <div className="skill" key={n}>
          <Switch
            label={`Skill ${n}`}
            checked={value.skills.includes(n)}
            onChange={(on) =>
              onChange({ ...value, skills: inLibraryOrder(on ? [...value.skills, n] : value.skills.filter((x) => x !== n), library) })
            }
          />
          <b className="grow">{n}</b>
          <span className="src">{library.includes(n) ? "library" : "not in the library"}</span>
        </div>
      ))}
      <label htmlFor={`${id}-model`}>Model</label>
      <select id={`${id}-model`} value={value.modelId} onChange={(e) => onChange({ ...value, modelId: e.target.value })}>
        <option value="">{def ? `Default model (${label(def)})` : "Default model"}</option>
        {choices.map((m) => (
          <option key={m.id} value={m.id}>
            {label(m)}
          </option>
        ))}
        {models.isSuccess && value.modelId !== "" && !choices.some((m) => m.id === value.modelId) && (
          <option value={value.modelId}>{value.modelId} (no longer in the list)</option>
        )}
      </select>
      {models.isError && <ErrorBox error={models.error} />}
      <label htmlFor={`${id}-callback`}>Callback URL</label>
      <input
        id={`${id}-callback`}
        type="text"
        autoComplete="off"
        spellCheck={false}
        maxLength={2000}
        placeholder="https://example.com/hooks/pergent"
        value={value.callbackUrl}
        onChange={(e) => onChange({ ...value, callbackUrl: e.target.value })}
      />
      <div className="meta">Empty means no callbacks: the caller reads each review with GET. Only http and https are allowed.</div>
    </>
  );
}
