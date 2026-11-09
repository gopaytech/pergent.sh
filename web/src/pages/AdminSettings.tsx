import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";

import {
  api,
  ApiError,
  skillBudget,
  type CommentFooterView,
  type DefaultsView,
  type LibrarySkill,
  type ModelAdminView,
  type ModelInUseView,
  type ModelsAdminView,
} from "../api";
import CommentLayoutEditor from "../CommentLayoutEditor";
import { useWrite } from "../layout/write";
import { ErrorBox, Loading, Switch, bytesText, noFill, secretFill, secretText } from "../ui";

// AdminSettings holds what an admin sets for the whole of pergent.sh: the
// defaults a repository switched on in the dashboard gets, and the list of
// models reviews can run on.
export default function AdminSettings() {
  const defaults = useQuery({ queryKey: ["admin-defaults"], queryFn: () => api.get<DefaultsView>("/api/admin/defaults") });
  const footer = useQuery({ queryKey: ["admin-comment-footer"], queryFn: () => api.get<CommentFooterView>("/api/admin/comment-footer") });
  const models = useQuery({ queryKey: modelsKey, queryFn: () => api.get<ModelsAdminView>("/api/admin/models") });
  return (
    <div className="narrow">
      <div className="top">
        <div>
          <h1>Settings</h1>
          <div className="sub">What a repository starts with, the review comment's footer, and the models reviews run on.</div>
        </div>
      </div>
      {defaults.isPending && <Loading />}
      {defaults.isError && <ErrorBox error={defaults.error} />}
      {defaults.isSuccess && <Defaults v={defaults.data} />}
      {footer.isPending && <Loading />}
      {footer.isError && <ErrorBox error={footer.error} />}
      {footer.isSuccess && <CommentFooter v={footer.data} />}
      {models.isPending && <Loading />}
      {models.isError && <ErrorBox error={models.error} />}
      {models.isSuccess && <Models v={models.data} />}
    </div>
  );
}

// --- the comment footer ---

function CommentFooter({ v }: { v: CommentFooterView }) {
  const client = useQueryClient();
  const write = useWrite();
  const [text, setText] = useState(v.note);
  const [note, setNote] = useState("");
  const save = useMutation({
    mutationFn: (next: string) => write<CommentFooterView>("PUT", "/api/admin/comment-footer", { note: next }),
    onMutate: () => setNote(""),
    onSuccess: (saved) => {
      client.setQueryData(["admin-comment-footer"], saved);
      setText(saved.note);
      setNote("Comment footer saved.");
    },
  });
  const submit = (e: FormEvent) => {
    e.preventDefault();
    save.mutate(text.trim());
  };
  return (
    <form className="card pad stack" aria-label="Comment footer" onSubmit={submit}>
      <h2>Comment footer</h2>
      <label htmlFor="comment-footer-note">Text</label>
      <input
        id="comment-footer-note"
        type="text"
        {...noFill}
        placeholder={v.default}
        maxLength={200}
        value={text}
        onChange={(e) => setText(e.target.value)}
      />
      <div className="meta">
        One line at the bottom of every review comment, from the next comment on. Markdown links work. An @ shows as plain text and notifies
        nobody. Empty uses the default.
      </div>
      {save.isError && <ErrorBox error={save.error} />}
      <div className="foot">
        {note && (
          <span className="meta grow" role="status">
            {note}
          </span>
        )}
        <button type="submit" className="btn primary" disabled={save.isPending}>
          Save footer
        </button>
      </div>
    </form>
  );
}

// --- the defaults ---

// FromConfig ends a default's help line: what the config gives while the
// default follows it, or, once a value was saved here, that value and a
// button to follow the config again.
function FromConfig({ field, pinned, shown, onUse, disabled }: { field: string; pinned: boolean; shown: string; onUse: () => void; disabled: boolean }) {
  if (!pinned) return <span> From config: {shown}</span>;
  return (
    <>
      <span> Saved here; the config says {shown}.</span>{" "}
      <button type="button" className="btn small" aria-label={`Use config value for ${field}`} disabled={disabled} onClick={onUse}>
        Use config value
      </button>
    </>
  );
}

// The label the status select shows for a value.
const statusText = (v: string) => (v === "failed" ? "failed (blocks merge)" : v);
const onOff = (on: boolean) => (on ? "on" : "off");
// A cap of 0 would be the built-in one; the form shows it empty.
const capText = (n: number) => (n > 0 ? String(n) : "built-in");
const capValue = (n: number) => (n > 0 ? String(n) : "");

// pinnedLayout is the layout saved here, or null while the built-in one applies.
const pinnedLayout = (v: DefaultsView) => (v.pinned.commentLayout === true ? v.commentLayout : null);

function Defaults({ v }: { v: DefaultsView }) {
  const client = useQueryClient();
  const write = useWrite();
  const [approve, setApprove] = useState(v.approve);
  const [reviewDrafts, setReviewDrafts] = useState(v.reviewDrafts);
  const [inlineFindings, setInlineFindings] = useState(v.inlineFindings);
  const [commandsOnly, setCommandsOnly] = useState(v.commandsAuthorAndMaintainersOnly);
  const [incremental, setIncremental] = useState(v.incrementalReviews);
  const [learn, setLearn] = useState(v.learnFromDismissals);
  const [quiet, setQuiet] = useState(v.quietReviews);
  const [linked, setLinked] = useState(v.linkedReposEnabled);
  // null is the built-in layout, which is all the config gives.
  const [layout, setLayout] = useState<string[] | null>(pinnedLayout(v));
  const [failureStatus, setFailureStatus] = useState(v.failureStatus);
  const [timeout, setTimeoutText] = useState(String(v.jobTimeoutSeconds));
  const [steps, setSteps] = useState(String(v.maxTurns));
  const [memory, setMemory] = useState(v.jobMemory);
  const [capMR, setCapMR] = useState(String(v.rerunLimitPerMR));
  const [capRepo, setCapRepo] = useState(String(v.rerunLimitPerRepo));
  const [skills, setSkills] = useState(v.skills);
  const cfg = v.config;
  const pinned = (key: string) => v.pinned[key] === true;
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
    // A timeout equal to the config's is sent as null, so the config owns its range.
    if (!/^\d+$/.test(t) || (Number(t) !== cfg.jobTimeoutSeconds && (Number(t) < 60 || Number(t) > 7200))) {
      setProblem("The timeout is a whole number of seconds, from 60 to 7200.");
      return;
    }
    const s = steps.trim();
    if (!/^\d+$/.test(s) || Number(s) < 5 || Number(s) > 200) {
      setProblem("Max steps is a whole number from 5 to 200.");
      return;
    }
    // A cap is a whole number from 1 to 1000; empty is the built-in one.
    const caps = [capMR, capRepo].map((c) => c.trim());
    if (caps.some((c) => c !== "" && (!/^\d+$/.test(c) || Number(c) < 1 || Number(c) > 1000))) {
      setProblem("The re-run caps are whole numbers from 1 to 1000.");
      return;
    }
    setProblem("");
    // The PUT replaces every default. A value equal to the config's is sent
    // as null, so the default keeps following the config; so is an empty
    // cap or memory.
    const orNull = <T,>(value: T, fromConfig: T) => (value === fromConfig ? null : value);
    const cap = (text: string, fromConfig: number) => (text === "" ? null : orNull(Number(text), fromConfig));
    save.mutate({
      approve: orNull(approve, cfg.approve),
      reviewDrafts: orNull(reviewDrafts, cfg.reviewDrafts),
      inlineFindings: orNull(inlineFindings, cfg.inlineFindings),
      commandsAuthorAndMaintainersOnly: orNull(commandsOnly, cfg.commandsAuthorAndMaintainersOnly),
      incrementalReviews: orNull(incremental, cfg.incrementalReviews),
      learnFromDismissals: orNull(learn, cfg.learnFromDismissals),
      quietReviews: orNull(quiet, cfg.quietReviews),
      linkedReposEnabled: orNull(linked, cfg.linkedReposEnabled),
      commentLayout: layout === null || layout.join() === cfg.commentLayout.join() ? null : layout,
      failureStatus: orNull(failureStatus, cfg.failureStatus),
      jobTimeoutSeconds: orNull(Number(t), cfg.jobTimeoutSeconds),
      maxTurns: orNull(Number(s), cfg.maxTurns),
      jobMemory: memory.trim() === "" ? null : orNull(memory.trim(), cfg.jobMemory),
      rerunLimitPerMR: cap(caps[0], cfg.rerunLimitPerMR),
      rerunLimitPerRepo: cap(caps[1], cfg.rerunLimitPerRepo),
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
          <div className="help">
            When off, pergent still reviews and comments but never approves.
            <FromConfig field="Approve clean reviews" pinned={pinned("approve")} shown={onOff(cfg.approve)} disabled={save.isPending} onUse={() => setApprove(cfg.approve)} />
          </div>
        </div>
        <div>
          <Switch label="Approve clean reviews" checked={approve} disabled={save.isPending} onChange={setApprove} />
        </div>
      </div>
      <div className="set">
        <div>
          <div className="name">Review drafts</div>
          <div className="help">
            When off, a draft isn't reviewed until it is marked ready.
            <FromConfig
              field="Review drafts"
              pinned={pinned("reviewDrafts")}
              shown={onOff(cfg.reviewDrafts)}
              disabled={save.isPending}
              onUse={() => setReviewDrafts(cfg.reviewDrafts)}
            />
          </div>
        </div>
        <div>
          <Switch label="Review drafts" checked={reviewDrafts} disabled={save.isPending} onChange={setReviewDrafts} />
        </div>
      </div>
      <div className="set">
        <div>
          <div className="name">Inline findings</div>
          <div className="help">
            Post each finding as a comment on its lines, with a one-click fix when it is small. Off keeps every finding in the review comment.
            <FromConfig
              field="Inline findings"
              pinned={pinned("inlineFindings")}
              shown={onOff(cfg.inlineFindings)}
              disabled={save.isPending}
              onUse={() => setInlineFindings(cfg.inlineFindings)}
            />
          </div>
        </div>
        <div>
          <Switch label="Inline findings" checked={inlineFindings} disabled={save.isPending} onChange={setInlineFindings} />
        </div>
      </div>
      <div className="set">
        <div>
          <div className="name">Only the author and maintainers use commands</div>
          <div className="help">
            When on, only the author of a change and the repository&apos;s maintainers and owners can use pergent commands on it. When off, anyone with write access can.
            <FromConfig
              field="Only the author and maintainers use commands"
              pinned={pinned("commandsAuthorAndMaintainersOnly")}
              shown={onOff(cfg.commandsAuthorAndMaintainersOnly)}
              disabled={save.isPending}
              onUse={() => setCommandsOnly(cfg.commandsAuthorAndMaintainersOnly)}
            />
          </div>
        </div>
        <div>
          <Switch label="Only the author and maintainers use commands" checked={commandsOnly} disabled={save.isPending} onChange={setCommandsOnly} />
        </div>
      </div>
      <div className="set">
        <div>
          <div className="name">Incremental reviews</div>
          <div className="help">
            After a push, review only the files changed since the last review.
            <FromConfig
              field="Incremental reviews"
              pinned={pinned("incrementalReviews")}
              shown={onOff(cfg.incrementalReviews)}
              disabled={save.isPending}
              onUse={() => setIncremental(cfg.incrementalReviews)}
            />
          </div>
        </div>
        <div>
          <Switch label="Incremental reviews" checked={incremental} disabled={save.isPending} onChange={setIncremental} />
        </div>
      </div>
      <div className="set">
        <div>
          <div className="name">Learn from dismissals</div>
          <div className="help">
            Later reviews are told what people with write access rejected, and why.
            <FromConfig
              field="Learn from dismissals"
              pinned={pinned("learnFromDismissals")}
              shown={onOff(cfg.learnFromDismissals)}
              disabled={save.isPending}
              onUse={() => setLearn(cfg.learnFromDismissals)}
            />
          </div>
        </div>
        <div>
          <Switch label="Learn from dismissals" checked={learn} disabled={save.isPending} onChange={setLearn} />
        </div>
      </div>
      <div className="set">
        <div>
          <div className="name">Quiet reviews</div>
          <div className="help">
            Fewer nitpicks, no suggestion threads and a shorter comment. Blocking findings, failures and approval stay the
            same.
            <FromConfig
              field="Quiet reviews"
              pinned={pinned("quietReviews")}
              shown={onOff(cfg.quietReviews)}
              disabled={save.isPending}
              onUse={() => setQuiet(cfg.quietReviews)}
            />
          </div>
        </div>
        <div>
          <Switch label="Quiet reviews" checked={quiet} disabled={save.isPending} onChange={setQuiet} />
        </div>
      </div>
      <div className="set">
        <div>
          <div className="name">Comment layout</div>
          <div className="help">
            The blocks of the review comment, in order. A block that is not shown is left out.
            {pinned("commentLayout") ? (
              <span> Saved here: repositories switched on in the dashboard start with this layout.</span>
            ) : (
              <span> Not saved here: repositories switched on in the dashboard start with the built-in layout.</span>
            )}
          </div>
        </div>
        <div className="wide">
          <CommentLayoutEditor value={layout} defaults={cfg.commentLayout} disabled={save.isPending} onChange={setLayout} />
        </div>
      </div>
      <div className="set">
        <div>
          <div className="name">Linked repositories</div>
          <div className="help">
            Off by default. The impact check across a repository's linked repositories is on for repositories switched on in
            the dashboard that don't set it. The links themselves are set on each repository's page, by an admin.
            <FromConfig
              field="Linked repositories"
              pinned={pinned("linkedReposEnabled")}
              shown={onOff(cfg.linkedReposEnabled)}
              disabled={save.isPending}
              onUse={() => setLinked(cfg.linkedReposEnabled)}
            />
          </div>
        </div>
        <div>
          <Switch label="Linked repositories" checked={linked} disabled={save.isPending} onChange={setLinked} />
        </div>
      </div>
      <div className="set">
        <div>
          <div className="name">Status when a review fails</div>
          <div className="help">
            "neutral" won't block merging when the LLM provider is down.
            <FromConfig
              field="Status when a review fails"
              pinned={pinned("failureStatus")}
              shown={statusText(cfg.failureStatus)}
              disabled={save.isPending}
              onUse={() => setFailureStatus(cfg.failureStatus)}
            />
          </div>
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
          <div className="help">
            How long one review may run, in seconds (60 to 7200).
            <FromConfig
              field="Timeout"
              pinned={pinned("jobTimeoutSeconds")}
              shown={String(cfg.jobTimeoutSeconds)}
              disabled={save.isPending}
              onUse={() => setTimeoutText(String(cfg.jobTimeoutSeconds))}
            />
          </div>
        </div>
        <div>
          <input
            type="text"
            inputMode="numeric"
            aria-label="Timeout (seconds)"
            value={timeout}
            disabled={save.isPending}
            onChange={(e) => setTimeoutText(e.target.value)}
          />
        </div>
      </div>
      <div className="set">
        <div>
          <div className="name">Max steps</div>
          <div className="help">
            Steps each opencode run may take before pergent stops it; each step can call several tools.
            <FromConfig field="Max steps" pinned={pinned("maxTurns")} shown={String(cfg.maxTurns)} disabled={save.isPending} onUse={() => setSteps(String(cfg.maxTurns))} />
          </div>
        </div>
        <div>
          <input
            type="text"
            inputMode="numeric"
            aria-label="Max steps"
            value={steps}
            disabled={save.isPending}
            onChange={(e) => setSteps(e.target.value)}
          />
        </div>
      </div>
      <div className="set">
        <div>
          <div className="name">Memory limit</div>
          <div className="help">
            For example 6Gi (256Mi to 32Gi). Empty follows the config.
            <FromConfig
              field="Memory limit"
              pinned={pinned("jobMemory")}
              shown={cfg.jobMemory === "" ? "none" : cfg.jobMemory}
              disabled={save.isPending}
              onUse={() => setMemory(cfg.jobMemory)}
            />
          </div>
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
            <FromConfig
              field="Re-run cap per MR/PR"
              pinned={pinned("rerunLimitPerMR")}
              shown={capText(cfg.rerunLimitPerMR)}
              disabled={save.isPending}
              onUse={() => setCapMR(capValue(cfg.rerunLimitPerMR))}
            />
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
          <div className="help">
            The most re-runs across a whole repository in a UTC day, 1 to 1000.
            <FromConfig
              field="Re-run cap per repository"
              pinned={pinned("rerunLimitPerRepo")}
              shown={capText(cfg.rerunLimitPerRepo)}
              disabled={save.isPending}
              onUse={() => setCapRepo(capValue(cfg.rerunLimitPerRepo))}
            />
          </div>
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

// --- the models ---

const modelsKey = ["admin-models"];

const labelOf = (m: { provider: string; model: string }) => `${m.provider} / ${m.model}`;
const repoText = (r: { instance: string; project: string }) => `${r.instance}: ${r.project}`;
const repositoriesText = (n: number) => `${n} ${n === 1 ? "repository" : "repositories"}`;

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

// Models is the list reviews can run on: the default, the ones a repository
// can be given, and the form to add one. Keys are write-only: they are sent
// when typed and never shown.
function Models({ v }: { v: ModelsAdminView }) {
  return (
    <>
      <section className="card" aria-label="Models">
        <h2>Models</h2>
        <div className="pad meta">
          A repository runs on the default model unless an admin of the repository picks another one on its Settings tab.
        </div>
        {!v.canSetDefault && (
          <div className="pad meta">
            The config file has an llm: block, which is the default; change it there. The models below can still be added,
            changed and removed.
          </div>
        )}
        {v.models.length === 0 && <div className="pad meta">No model yet. Add one below; the first is the default.</div>}
        {v.models.map((m) => (
          <ModelRow key={m.id} m={m} canSetDefault={v.canSetDefault} following={v.followingDefault} />
        ))}
      </section>
      <AddModel />
    </>
  );
}

// inUseBody is the 409 body of a removal of a model that repositories name,
// or null for any other failure.
function inUseBody(err: unknown): ModelInUseView | null {
  if (!(err instanceof ApiError) || err.status !== 409) return null;
  const b = err.body as Partial<ModelInUseView> | undefined;
  if (!b || !Array.isArray(b.repositories)) return null;
  return { error: err.message, repositories: b.repositories };
}

function ModelRow({ m, canSetDefault, following }: { m: ModelAdminView; canSetDefault: boolean; following: number }) {
  const client = useQueryClient();
  const write = useWrite();
  const [editing, setEditing] = useState(false);
  const label = labelOf(m);
  // A change of the list moves what the repository pages offer.
  const changed = () => {
    client.invalidateQueries({ queryKey: modelsKey });
    client.invalidateQueries({ queryKey: ["repo"] });
  };
  const makeDefault = useMutation({
    mutationFn: () => write<ModelsAdminView>("POST", `/api/admin/models/${encodeURIComponent(m.id)}/default`, {}),
    onSuccess: (list) => {
      client.setQueryData(modelsKey, list);
      client.invalidateQueries({ queryKey: ["repo"] });
    },
  });
  const remove = useMutation({
    mutationFn: () => write<void>("DELETE", `/api/admin/models/${encodeURIComponent(m.id)}`),
    onSuccess: changed,
  });
  const inUse = remove.isError ? inUseBody(remove.error) : null;
  const fromConfig = m.source === "config";
  const busy = makeDefault.isPending || remove.isPending;
  return (
    <div className="pad stack" role="group" aria-label={label}>
      <div className="row">
        <b className="grow mono">{label}</b>
        {m.default && <span className="badge">default</span>}
        {fromConfig && <span className="badge cfg">from the config file</span>}
      </div>
      {m.baseURL && <div className="meta">Base URL {m.baseURL}</div>}
      <div className="meta">Key {secretText(m.key)}</div>
      {m.repositories.length > 0 && <div className="meta">Used by {m.repositories.map(repoText).join(", ")}</div>}
      {m.default && following > 0 && (
        <div className="meta">
          {repositoriesText(following)} name no model and follow it.
        </div>
      )}
      {!fromConfig && !editing && (
        <div className="actions-row">
          <button type="button" className="btn small" aria-label={`Edit ${label}`} disabled={busy} onClick={() => setEditing(true)}>
            Edit
          </button>
          {canSetDefault && !m.default && (
            <button
              type="button"
              className="btn small"
              aria-label={`Set ${label} as default`}
              disabled={busy}
              onClick={() => {
                const moved =
                  following === 0
                    ? "No repository follows the default today."
                    : `The ${repositoriesText(following)} that name no model will review with it from now on.`;
                if (window.confirm(`Make ${label} the default model? ${moved}`)) makeDefault.mutate();
              }}
            >
              Set as default
            </button>
          )}
          {!m.default && (
            <button
              type="button"
              className="btn small"
              aria-label={`Remove ${label}`}
              disabled={busy}
              onClick={() => {
                if (window.confirm(`Remove ${label} from the list? Its key is deleted.`)) remove.mutate();
              }}
            >
              Remove
            </button>
          )}
        </div>
      )}
      {!fromConfig && m.default && !editing && <div className="meta">Make another model the default to remove this one.</div>}
      {editing && <EditModel m={m} onDone={() => setEditing(false)} onSaved={changed} />}
      {makeDefault.isError && <ErrorBox error={makeDefault.error} />}
      {remove.isError &&
        (inUse ? (
          <div className="error" role="alert">
            {label} is in use. Change the model of these repositories first:
            <ul>
              {inUse.repositories.map((r) => (
                <li key={repoText(r)}>{repoText(r)}</li>
              ))}
            </ul>
          </div>
        ) : (
          <ErrorBox error={remove.error} />
        ))}
    </div>
  );
}

// EditModel changes the base URL of an entry and, when typed, its key. The
// provider and model never change: another one is a new entry.
function EditModel({ m, onDone, onSaved }: { m: ModelAdminView; onDone: () => void; onSaved: () => void }) {
  const write = useWrite();
  const label = labelOf(m);
  const [baseURL, setBaseURL] = useState(m.baseURL);
  const [key, setKey] = useState("");
  // With a key stored, its input waits for Replace; with none, it is offered.
  const [replacing, setReplacing] = useState(false);
  const askKey = !m.key.set || replacing;
  const save = useMutation({
    mutationFn: (body: Record<string, string>) => write<ModelAdminView>("PUT", `/api/admin/models/${encodeURIComponent(m.id)}`, body),
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
    save.mutate({ baseURL: baseURL.trim(), ...(askKey && key !== "" ? { apiKey: key } : {}) });
  };
  const id = `model-${m.id}`;
  return (
    <form className="stack" aria-label={`Edit ${label}`} onSubmit={submit}>
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
      <div className="meta">Empty uses the provider's own API. Set it for a proxy or an OpenAI-compatible server.</div>
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
      <div className="actions-row">
        <button type="submit" className="btn primary small" disabled={save.isPending}>
          Save
        </button>
        <button type="button" className="btn small" disabled={save.isPending} onClick={onDone}>
          Cancel
        </button>
      </div>
    </form>
  );
}

// AddModel adds an entry to the list. The key is write-only: it is sent
// once and the input is emptied.
function AddModel() {
  const client = useQueryClient();
  const write = useWrite();
  const [choice, setChoice] = useState("");
  const [otherID, setOtherID] = useState("");
  const [model, setModel] = useState("");
  const [baseURL, setBaseURL] = useState("");
  const [key, setKey] = useState("");
  const [note, setNote] = useState("");
  const add = useMutation({
    mutationFn: (body: Record<string, string>) => write<ModelAdminView>("POST", "/api/admin/models", body),
    // The closure holds the key: drop it with the input, not after 5 minutes.
    gcTime: 0,
    onMutate: () => setNote(""),
    onSuccess: (added) => {
      setChoice("");
      setOtherID("");
      setModel("");
      setBaseURL("");
      setKey("");
      setNote(`Added ${labelOf(added)}.`);
      // The first entry is the default: the list says which.
      client.invalidateQueries({ queryKey: modelsKey });
      client.invalidateQueries({ queryKey: ["repo"] });
    },
  });
  const submit = (e: FormEvent) => {
    e.preventDefault();
    add.mutate({
      provider: (choice === "other" ? otherID : choice).trim(),
      model: model.trim(),
      baseURL: baseURL.trim(),
      apiKey: key,
    });
  };
  return (
    <form className="card pad stack" aria-label="Add a model" onSubmit={submit}>
      <h2>Add a model</h2>
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
      {add.isError && <ErrorBox error={add.error} />}
      {note && (
        <div className="note" role="status">
          {note}
        </div>
      )}
      <div className="actions-row">
        <button type="submit" className="btn primary" disabled={add.isPending}>
          Add model
        </button>
      </div>
    </form>
  );
}
