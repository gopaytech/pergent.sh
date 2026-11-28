import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { NativeSelect } from "@/components/ui/native-select";
import { cn } from "@/lib/utils";

import {
  api,
  paths,
  skillBudget,
  type ChainEntryView,
  type ChainRef,
  type ChainView,
  type CommentFooterView,
  type DefaultsView,
  type LibrarySkill,
  type RuleSet,
  type ModelAdminView,
  type ModelsAdminView,
  type TeamLimitView,
  type TeamView,
} from "../api";
import ChainEditor from "../ChainEditor";
import LimitField from "../LimitField";
import CommentLayoutEditor from "../CommentLayoutEditor";
import { FoldHead, countText } from "../Fold";
import { useWrite } from "../layout/write";
import RuleSetPicker from "../RuleSetPicker";
import MarkBanner, { markText } from "../MarkBanner";
import { AddModelForm, EditModelForm, windowText } from "../ModelForms";
import { QuotaLine, parseQuota, quotaMax } from "../quota";
import { blockingHelp, blockingLabel, failureHelp, failureLabel, notUsedNote } from "../statusSettings";
import { ErrorBox, Loading, PageHeader, Switch, bytesText, noFill, secretText } from "../ui";

// The looks this page repeats. A folding card keeps "card" and "folded":
// FoldHead's title is drawn by them, and "folded" says the card is shut.
const foldCard = (open: boolean) => cn("card mb-4 gap-0 overflow-hidden py-0", !open && "folded");
// A setting is a row: its name and help on the left, its control on the
// right. "set" and "name" mark them for the tests.
const setRow = "set grid grid-cols-1 items-center gap-4 border-b px-4 py-3.5 md:grid-cols-[1fr_280px]";
const setName = "name text-sm font-medium";
const setHelp = "text-muted-foreground mt-0.5 text-sm";
// A setting whose control needs the whole width, on a line of its own.
const setWide = "col-span-full -mx-4 -mb-3.5";
// A line of text in a card, and the save bar at its foot.
const cardText = "text-muted-foreground px-4 py-3 text-sm";
const foot = "bg-muted/40 flex items-center justify-end gap-2 border-t px-4 py-3";
const hint = "text-muted-foreground text-sm";

// AdminSettings holds what an admin sets for the whole of pergent.sh: the
// defaults a repository switched on in the dashboard gets, and the list of
// models reviews can run on.
export default function AdminSettings() {
  const defaults = useQuery({ queryKey: ["admin-defaults"], queryFn: () => api.get<DefaultsView>("/api/admin/defaults") });
  const footer = useQuery({ queryKey: ["admin-comment-footer"], queryFn: () => api.get<CommentFooterView>("/api/admin/comment-footer") });
  const models = useQuery({ queryKey: modelsKey, queryFn: () => api.get<ModelsAdminView>("/api/admin/models") });
  return (
    <div className="max-w-[1100px]">
      <PageHeader title="Settings" description="What a repository starts with, the review comment's footer, and the models reviews run on." />
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
  const [open, setOpen] = useState(false);
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
    <form aria-label="Comment footer" onSubmit={submit}>
      <Card className={foldCard(open)}>
        <FoldHead title="Comment footer" summary={v.note === "" ? "The default text" : v.note} open={open} onToggle={() => setOpen(!open)} />
        <div hidden={!open}>
          <div className="grid gap-2 p-4">
            <Label htmlFor="comment-footer-note">Text</Label>
            <Input
              id="comment-footer-note"
              type="text"
              {...noFill}
              placeholder={v.default}
              maxLength={200}
              value={text}
              onChange={(e) => setText(e.target.value)}
            />
            <div className={hint}>
              One line at the bottom of every review comment, from the next comment on. Markdown links work. An @ shows as plain text and
              notifies nobody. Empty uses the default.
            </div>
            {save.isError && <ErrorBox error={save.error} />}
          </div>
          <div className={foot}>
            {note && (
              <span className="text-muted-foreground min-w-0 flex-1 text-sm" role="status">
                {note}
              </span>
            )}
            <Button type="submit" size="sm" disabled={save.isPending}>
              Save footer
            </Button>
          </div>
        </div>
      </Card>
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
      <Button
        type="button"
        variant="outline"
        size="sm"
        className="h-7 px-2 text-xs"
        aria-label={`Use config value for ${field}`}
        disabled={disabled}
        onClick={onUse}
      >
        Use config value
      </Button>
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

// defaultsSummary is the folded card's line: how many defaults were set
// here, and how many default skills and rule sets there are.
function defaultsSummary(v: DefaultsView) {
  const set = Object.values(v.pinned).filter((p) => p === true).length;
  return [
    set === 0 ? "All follow the config" : `${set} set here`,
    countText(v.skills.length, "default skill"),
    countText((v.ruleSets ?? []).length, "default rule set"),
  ].join(" · ");
}

function Defaults({ v }: { v: DefaultsView }) {
  const client = useQueryClient();
  const write = useWrite();
  // Open at first: it is what an admin comes to this page for.
  const [open, setOpen] = useState(true);
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
  const [blockingStatus, setBlockingStatus] = useState(v.blockingStatus);
  const [timeout, setTimeoutText] = useState(String(v.jobTimeoutSeconds));
  const [steps, setSteps] = useState(String(v.maxTurns));
  const [memory, setMemory] = useState(v.jobMemory);
  const [capMR, setCapMR] = useState(String(v.rerunLimitPerMR));
  const [capRepo, setCapRepo] = useState(String(v.rerunLimitPerRepo));
  const [skills, setSkills] = useState(v.skills);
  const [defaultRuleSets, setDefaultRuleSets] = useState(v.ruleSets ?? []);
  const cfg = v.config;
  const pinned = (key: string) => v.pinned[key] === true;
  const [problem, setProblem] = useState("");
  const [note, setNote] = useState("");
  const library = useQuery({ queryKey: ["skills"], queryFn: () => api.get<LibrarySkill[]>("/api/skills") });
  const ruleLibrary = useQuery({ queryKey: ["rule-sets"], queryFn: () => api.get<RuleSet[]>("/api/rule-sets") });
  const save = useMutation({
    mutationFn: (body: Record<string, unknown>) => write<DefaultsView>("PUT", "/api/admin/defaults", body),
    onMutate: () => setNote(""),
    onSuccess: (saved) => {
      client.setQueryData(["admin-defaults"], saved);
      // The library's "default" marks and the repository pages' default list
      // come from /api/skills, and the rule library's from /api/rule-sets.
      client.invalidateQueries({ queryKey: ["skills"] });
      client.invalidateQueries({ queryKey: ["rule-sets"] });
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
      blockingStatus: orNull(blockingStatus, cfg.blockingStatus),
      jobTimeoutSeconds: orNull(Number(t), cfg.jobTimeoutSeconds),
      maxTurns: orNull(Number(s), cfg.maxTurns),
      jobMemory: memory.trim() === "" ? null : orNull(memory.trim(), cfg.jobMemory),
      rerunLimitPerMR: cap(caps[0], cfg.rerunLimitPerMR),
      rerunLimitPerRepo: cap(caps[1], cfg.rerunLimitPerRepo),
      // Left out when the library couldn't be read, the stored list is kept.
      ...(library.isSuccess ? { skills: library.data.map((s) => s.name).filter((n) => skills.includes(n)) } : {}),
      // Left out when the rule library couldn't be read, the stored list is
      // kept. A name no longer in the library is dropped, as for skills.
      ...(ruleLibrary.isSuccess ? { ruleSets: defaultRuleSets.filter((n) => ruleLibrary.data.some((s) => s.name === n)) } : {}),
    });
  };
  return (
    <form aria-label="Repository defaults" noValidate onSubmit={submit}>
      <Card className={foldCard(open)}>
        <FoldHead title="Repository defaults" summary={defaultsSummary(v)} open={open} onToggle={() => setOpen(!open)} />
        <div hidden={!open}>
          <div className={cn(cardText, "border-b")}>
            What a repository you switch on in the dashboard starts with. A repository can override each in its own settings;
            those listed in the config file keep the config's.
          </div>
          <div className={setRow}>
            <div>
              <div className={setName}>Approve clean reviews</div>
              <div className={setHelp}>
                When off, pergent still reviews and comments but never approves.
                <FromConfig field="Approve clean reviews" pinned={pinned("approve")} shown={onOff(cfg.approve)} disabled={save.isPending} onUse={() => setApprove(cfg.approve)} />
              </div>
            </div>
            <div>
              <Switch label="Approve clean reviews" checked={approve} disabled={save.isPending} onChange={setApprove} />
            </div>
          </div>
          <div className={setRow}>
            <div>
              <div className={setName}>Review drafts</div>
              <div className={setHelp}>
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
          <div className={setRow}>
            <div>
              <div className={setName}>Inline findings</div>
              <div className={setHelp}>
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
          <div className={setRow}>
            <div>
              <div className={setName}>Only the author and maintainers use commands</div>
              <div className={setHelp}>
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
          <div className={setRow}>
            <div>
              <div className={setName}>Incremental reviews</div>
              <div className={setHelp}>
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
          <div className={setRow}>
            <div>
              <div className={setName}>Learn from dismissals</div>
              <div className={setHelp}>
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
          <div className={setRow}>
            <div>
              <div className={setName}>Quiet reviews</div>
              <div className={setHelp}>
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
          <div className={setRow}>
            <div>
              <div className={setName}>Comment layout</div>
              <div className={setHelp}>
                The blocks of the review comment, in order. A block that is not shown is left out.
                {pinned("commentLayout") ? (
                  <span> Saved here: repositories switched on in the dashboard start with this layout.</span>
                ) : (
                  <span> Not saved here: repositories switched on in the dashboard start with the built-in layout.</span>
                )}
              </div>
            </div>
            <div className={setWide}>
              <CommentLayoutEditor value={layout} defaults={cfg.commentLayout} disabled={save.isPending} onChange={setLayout} />
            </div>
          </div>
          <div className={setRow}>
            <div>
              <div className={setName}>Linked repositories</div>
              <div className={setHelp}>
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
          <div className={setRow}>
            <div>
              <div className={setName}>{blockingLabel}</div>
              <div className={setHelp}>
                {blockingHelp}
                <FromConfig
                  field={blockingLabel}
                  pinned={pinned("blockingStatus")}
                  shown={statusText(cfg.blockingStatus)}
                  disabled={save.isPending}
                  onUse={() => setBlockingStatus(cfg.blockingStatus)}
                />
              </div>
            </div>
            <div>
              <NativeSelect aria-label={blockingLabel} value={blockingStatus} disabled={save.isPending} onChange={(e) => setBlockingStatus(e.target.value)}>
                <option value="failed">failed (blocks merge)</option>
                <option value="neutral">neutral</option>
              </NativeSelect>
            </div>
          </div>
          <div className={setRow}>
            <div>
              <div className={setName}>{failureLabel}</div>
              <div className={setHelp}>
                {failureHelp}
                <FromConfig
                  field={failureLabel}
                  pinned={pinned("failureStatus")}
                  shown={statusText(cfg.failureStatus)}
                  disabled={save.isPending}
                  onUse={() => setFailureStatus(cfg.failureStatus)}
                />
              </div>
            </div>
            <div>
              <NativeSelect aria-label={failureLabel} value={failureStatus} disabled={save.isPending} onChange={(e) => setFailureStatus(e.target.value)}>
                <option value="failed">failed (blocks merge)</option>
                <option value="neutral">neutral</option>
              </NativeSelect>
              {blockingStatus === "neutral" && <div className="text-muted-foreground mt-1 text-xs">{notUsedNote}</div>}
            </div>
          </div>
          <div className={setRow}>
            <div>
              <div className={setName}>Timeout</div>
              <div className={setHelp}>
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
              <Input
                type="text"
                inputMode="numeric"
                aria-label="Timeout (seconds)"
                value={timeout}
                disabled={save.isPending}
                onChange={(e) => setTimeoutText(e.target.value)}
              />
            </div>
          </div>
          <div className={setRow}>
            <div>
              <div className={setName}>Max steps</div>
              <div className={setHelp}>
                Steps each review run may take before pergent stops it; each step can call several tools.
                <FromConfig field="Max steps" pinned={pinned("maxTurns")} shown={String(cfg.maxTurns)} disabled={save.isPending} onUse={() => setSteps(String(cfg.maxTurns))} />
              </div>
            </div>
            <div>
              <Input
                type="text"
                inputMode="numeric"
                aria-label="Max steps"
                value={steps}
                disabled={save.isPending}
                onChange={(e) => setSteps(e.target.value)}
              />
            </div>
          </div>
          <div className={setRow}>
            <div>
              <div className={setName}>Memory limit</div>
              <div className={setHelp}>
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
              <Input
                type="text"
                aria-label="Memory limit"
                placeholder="none"
                value={memory}
                disabled={save.isPending}
                onChange={(e) => setMemory(e.target.value)}
              />
            </div>
          </div>
          <div className={setRow}>
            <div>
              <div className={setName}>Re-run cap per MR/PR</div>
              <div className={setHelp}>
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
              <Input
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
          <div className={setRow}>
            <div>
              <div className={setName}>Re-run cap per repository</div>
              <div className={setHelp}>
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
              <Input
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
          <div className={setRow}>
            <div>
              <div className={setName}>Default skills</div>
              <div className={setHelp}>
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
          <div className={setRow}>
            <div>
              <div className={setName}>Default rule sets</div>
              <div className={setHelp}>
                The rule sets, in match order, of a repository that doesn't choose its own. The first entry that matches a file gives
                it its rule.
              </div>
            </div>
          </div>
          {ruleLibrary.isPending && <Loading />}
          {ruleLibrary.isError && <ErrorBox error={ruleLibrary.error} />}
          {ruleLibrary.isSuccess && (
            <RuleSetPicker
              label="Default rule sets"
              library={ruleLibrary.data}
              selected={defaultRuleSets}
              disabled={save.isPending}
              onChange={setDefaultRuleSets}
            />
          )}
          {save.isError && <ErrorBox error={save.error} />}
          <div className={foot}>
            {problem && <span className="text-bad mr-auto text-sm">{problem}</span>}
            {note && (
              <span className="text-muted-foreground min-w-0 flex-1 text-sm" role="status">
                {note}
              </span>
            )}
            <Button type="submit" size="sm" disabled={save.isPending}>
              Save defaults
            </Button>
          </div>
        </div>
      </Card>
    </form>
  );
}

// skillRow is a library skill in the default list; "skill" marks it.
const skillRow = "skill flex items-center gap-2 border-b px-4 py-2.5 text-sm";

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
      {library.length === 0 && <div className={cn(cardText, "border-b")}>The library has no skills yet. Add some under Skills.</div>}
      {library.map((s) => (
        <div className={skillRow} key={s.name}>
          <Switch
            label={`Default skill ${s.name}`}
            checked={selected.includes(s.name)}
            disabled={disabled}
            onChange={(on) => onChange(on ? [...selected, s.name] : selected.filter((n) => n !== s.name))}
          />
          <b className="min-w-0 grow font-semibold">{s.name}</b>
          <Badge variant="outline" className="text-muted-foreground font-normal">
            {bytesText(s.size)}
          </Badge>
        </div>
      ))}
      {missing.map((n) => (
        <div className={skillRow} key={`missing/${n}`}>
          <Switch
            label={`Default skill ${n}`}
            checked={selected.includes(n)}
            disabled={disabled}
            onChange={(on) => onChange(on ? [...selected, n] : selected.filter((x) => x !== n))}
          />
          <b className="min-w-0 grow font-semibold">{n}</b>
          <span className="text-bad text-sm">not in the library</span>
        </div>
      ))}
      {missing.some((n) => selected.includes(n)) && (
        <div className={cardText}>
          {missing.filter((n) => selected.includes(n)).join(", ")} {missing.length === 1 ? "is" : "are"} no longer in the library.
          Saving removes {missing.length === 1 ? "it" : "them"} from the default skills.
        </div>
      )}
      {/* "problem" marks a total over the budget. */}
      <div className={cn("px-4 py-3 text-sm", over ? "problem text-bad" : "text-muted-foreground")}>
        Total {bytesText(total)} of {bytesText(skillBudget)}
      </div>
      {over && <div className="text-bad px-4 pb-3 text-sm">A review takes at most that much library skill text; the save will be refused.</div>}
      {selected.length === 0 && (
        <div className={cardText}>
          With no default skills, a repository that doesn't choose its own gets no skills and is not reviewed.
        </div>
      )}
    </div>
  );
}

// --- the models ---

const modelsKey = ["admin-models"];
const chainKey = ["admin-chain"];

const repositoriesText = (n: number) => `${n} ${n === 1 ? "repository" : "repositories"}`;

// Models is the list reviews can run on: the default, the global chain, and
// the form to add an entry. Keys are write-only: they are sent when typed
// and never shown.
function Models({ v }: { v: ModelsAdminView }) {
  const client = useQueryClient();
  const [open, setOpen] = useState(false);
  const [adding, setAdding] = useState(false);
  return (
    <>
      <Card role="region" className={foldCard(open)} aria-label="Models">
        <FoldHead title="Models" summary={modelsSummary(v)} open={open} onToggle={() => setOpen(!open)}>
          {!adding && (
            <Button
              type="button"
              variant="outline"
              size="sm"
              onClick={() => {
                setOpen(true);
                setAdding(true);
              }}
            >
              Add model
            </Button>
          )}
        </FoldHead>
        <div hidden={!open}>
          <MarkBanner entries={v.models} />
          <div className={cardText}>
            Reviews run on the global chain below, unless a repository's owner team or the repository has a chain of its own. A
            team chain may use only the entries with teams may use on.
          </div>
          {!v.canSetDefault && (
            <div className={cardText}>
              The config file has an llm: block, which is the default; change it there. The models below can still be added,
              changed and removed.
            </div>
          )}
          {v.models.length === 0 && <div className={cardText}>No model yet. Add one with Add model; the first is the default.</div>}
          {v.models.map((m) => (
            <ModelRow key={m.id} m={m} canSetDefault={v.canSetDefault} following={v.followingDefault} windowOff={v.windowOff} />
          ))}
          {adding && (
            <AddModelForm<ModelAdminView>
              path="/api/admin/models"
              teamsMayUse
              windowOff={v.windowOff}
              onCancel={() => setAdding(false)}
              onAdded={() => {
                // The first entry is the default: the list says which.
                client.invalidateQueries({ queryKey: modelsKey });
                client.invalidateQueries({ queryKey: chainKey });
                client.invalidateQueries({ queryKey: ["repo-reviewers"] });
                client.invalidateQueries({ queryKey: ["repo"] });
              }}
            />
          )}
        </div>
      </Card>
      <GlobalChain />
    </>
  );
}

// modelsSummary is the folded Models card's line: how many models, which
// is the default, and how many are marked.
function modelsSummary(v: ModelsAdminView) {
  if (v.models.length === 0) return "No models";
  const def = v.models.find((m) => m.default);
  const marked = v.models.filter((m) => m.mark?.live).length;
  return [countText(v.models.length, "model"), def ? `default ${def.name}` : "", marked > 0 ? `${marked} marked` : ""]
    .filter((p) => p !== "")
    .join(" · ");
}

// tokensText is a model's tokens this month, against its limit when it has one.
const fmt = (n: number) => n.toLocaleString("en-US");
const tokensText = (used: number, limit: number | null) => `${fmt(used)}${limit === null ? "" : ` of ${fmt(limit)}`} tokens this month`;

function ModelRow({
  m,
  canSetDefault,
  following,
  windowOff,
}: {
  m: ModelAdminView;
  canSetDefault: boolean;
  following: number;
  windowOff: string;
}) {
  const client = useQueryClient();
  const write = useWrite();
  // A row shows its name, model and tokens; the rest opens on a click.
  const [open, setOpen] = useState(false);
  const [editing, setEditing] = useState(false);
  // A change of the list moves what the chains and repository pages show.
  const changed = () => {
    client.invalidateQueries({ queryKey: modelsKey });
    client.invalidateQueries({ queryKey: chainKey });
    client.invalidateQueries({ queryKey: ["repo-reviewers"] });
    client.invalidateQueries({ queryKey: ["repo"] });
    client.invalidateQueries({ queryKey: ["repo-chain"] });
  };
  // A limit moves the bars here, the chains' tags and the team pages.
  const limitsChanged = () => {
    changed();
    client.invalidateQueries({ queryKey: ["team-chain"] });
    client.invalidateQueries({ queryKey: ["team-limits"] });
  };
  const makeDefault = useMutation({
    mutationFn: () => write<ModelsAdminView>("POST", `/api/admin/models/${encodeURIComponent(m.id)}/default`, {}),
    onSuccess: (list) => {
      client.setQueryData(modelsKey, list);
      client.invalidateQueries({ queryKey: chainKey });
      client.invalidateQueries({ queryKey: ["repo-reviewers"] });
      client.invalidateQueries({ queryKey: ["repo"] });
    },
  });
  const remove = useMutation({
    mutationFn: () => write<void>("DELETE", `/api/admin/models/${encodeURIComponent(m.id)}`),
    onSuccess: changed,
  });
  // The PUT needs the base URL: it is sent as it is.
  const forTeams = useMutation({
    mutationFn: (on: boolean) => write<ModelAdminView>("PUT", `/api/admin/models/${encodeURIComponent(m.id)}`, { baseURL: m.baseURL, teamsMayUse: on }),
    onSuccess: changed,
  });
  const clear = useMutation({
    mutationFn: () => write<void>("DELETE", paths.apiAdminModelMark(m.id)),
    onSuccess: changed,
  });
  const fromConfig = m.source === "config";
  const busy = makeDefault.isPending || remove.isPending || forTeams.isPending;
  const confirmRemove = () => {
    const chains = m.usedIn.length > 0 ? ` These chains name it and will skip it: ${m.usedIn.join(", ")}.` : "";
    if (window.confirm(`Remove ${m.name} from the list? Its key is deleted.${chains}`)) remove.mutate();
  };
  return (
    <div className="border-t" role="group" aria-label={m.name}>
      {/* "fold" draws the arrow, as on a card's title. */}
      <button type="button" className="fold items-center text-sm font-normal" aria-expanded={open} data-fold="" onClick={() => setOpen(!open)}>
        <b className="font-semibold">{m.name}</b>
        {m.default && <Badge variant="run">default</Badge>}
        {fromConfig && <Badge variant="grey">from the config file</Badge>}
        {m.mark?.live && <Badge variant="warn">marked</Badge>}
        <span className="text-muted-foreground min-w-0 grow truncate font-mono text-xs">
          {m.provider} / {m.model}
        </span>
        {windowText(m.contextWindow, m.maxOutput) !== "" && (
          <span className="text-muted-foreground text-xs whitespace-nowrap" title="Context window / Max output">
            {windowText(m.contextWindow, m.maxOutput)}
          </span>
        )}
        <span className="text-muted-foreground text-xs whitespace-nowrap">{tokensText(m.used, m.limit)}</span>
      </button>
      <div className="grid justify-items-stretch gap-3 pr-4 pb-4 pl-9" hidden={!open}>
        {m.baseURL && <div className={hint}>Base URL {m.baseURL}</div>}
        <div className={hint}>Key {secretText(m.key)}</div>
        {m.default && following > 0 && (
          <div className={hint}>
            {repositoriesText(following)} name no model and follow it.
          </div>
        )}
        <QuotaLine ariaLabel={`Tokens of ${m.name} this month`} use={{ used: m.used, limit: m.limit }} />
        <LimitField
          key={String(m.limit)}
          label={`Model limit of ${m.name}`}
          saveLabel={`Save the model limit of ${m.name}`}
          hint="Tokens per calendar month (UTC): input, cache read, cache write, output and reasoning. On every instance and repository. Empty: no limit."
          limit={m.limit}
          path={paths.apiAdminModelLimit(m.id)}
          onSaved={limitsChanged}
        />
        <ModelTeamLimits m={m} onChanged={limitsChanged} />
        {m.mark?.live && (
          <div className="flex items-center gap-3">
            <span className="text-warn min-w-0 grow text-sm">Marked: {markText(m.mark)}</span>
            <Button
              type="button"
              variant="outline"
              size="sm"
              aria-label={`Clear mark of ${m.name}`}
              disabled={clear.isPending}
              onClick={() => clear.mutate()}
            >
              Clear mark
            </Button>
          </div>
        )}
        {!fromConfig && (
          <Label className="font-normal">
            <Switch label={`Teams may use ${m.name}`} checked={m.teamsMayUse} disabled={busy} onChange={(on) => forTeams.mutate(on)} />
            <span>Teams may use it</span>
          </Label>
        )}
        {!fromConfig && !editing && (
          <div className="flex flex-wrap gap-2">
            <Button type="button" variant="outline" size="sm" aria-label={`Edit ${m.name}`} disabled={busy} onClick={() => setEditing(true)}>
              Edit
            </Button>
            {canSetDefault && !m.default && (
              <Button
                type="button"
                variant="outline"
                size="sm"
                aria-label={`Set ${m.name} as default`}
                disabled={busy}
                onClick={() => {
                  const moved =
                    following === 0
                      ? "No repository follows the default today."
                      : `The ${repositoriesText(following)} that name no model will review with it from now on.`;
                  if (window.confirm(`Make ${m.name} the default model? ${moved}`)) makeDefault.mutate();
                }}
              >
                Set as default
              </Button>
            )}
            {!m.default && (
              <Button type="button" variant="destructive-outline" size="sm" aria-label={`Remove ${m.name}`} disabled={busy} onClick={confirmRemove}>
                Remove
              </Button>
            )}
          </div>
        )}
        {!fromConfig && m.default && !editing && <div className={hint}>Make another model the default to remove this one.</div>}
        {editing && (
          <EditModelForm m={m} path={`/api/admin/models/${encodeURIComponent(m.id)}`} windowOff={windowOff} onDone={() => setEditing(false)} onSaved={changed} />
        )}
        {makeDefault.isError && <ErrorBox error={makeDefault.error} />}
        {remove.isError && <ErrorBox error={remove.error} />}
        {forTeams.isError && <ErrorBox error={forTeams.error} />}
        {clear.isError && <ErrorBox error={clear.error} />}
      </div>
    </div>
  );
}

const addLimitProblem = `Pick a team and type its limit: a whole number from 1 to ${quotaMax}.`;

// ModelTeamLimits is each team's limit on a global entry, folded until opened:
// the limit and that team's tokens on the entry this month, changed or
// removed in place, and a form to add one for a team with none.
function ModelTeamLimits({ m, onChanged }: { m: ModelAdminView; onChanged: () => void }) {
  const [open, setOpen] = useState(false);
  return (
    <div className="grid justify-items-start gap-3">
      <Button type="button" variant="outline" size="sm" aria-expanded={open} onClick={() => setOpen(!open)}>
        Team limits ({m.teamLimits.length})
      </Button>
      {open && (
        <div className="grid w-full gap-3" role="group" aria-label={`Team limits on ${m.name}`}>
          {m.teamLimits.length === 0 && <div className={hint}>No team has a limit on it.</div>}
          {m.teamLimits.map((t) => (
            <TeamLimitRow key={t.teamId} m={m} t={t} onChanged={onChanged} />
          ))}
          <AddTeamLimit m={m} onAdded={onChanged} />
        </div>
      )}
    </div>
  );
}

function TeamLimitRow({ m, t, onChanged }: { m: ModelAdminView; t: TeamLimitView; onChanged: () => void }) {
  const write = useWrite();
  const remove = useMutation({
    mutationFn: () => write<void>("DELETE", paths.apiAdminModelTeamLimit(m.id, t.teamId)),
    onSuccess: () => onChanged(),
  });
  return (
    <div className="grid gap-2 rounded-md border p-3" role="group" aria-label={`Team ${t.team}`}>
      <div className="flex items-center gap-3">
        <b className="min-w-0 grow text-sm font-semibold">{t.team}</b>
        <Button
          type="button"
          variant="outline"
          size="sm"
          aria-label={`Remove the limit of ${t.team} on ${m.name}`}
          disabled={remove.isPending}
          onClick={() => remove.mutate()}
        >
          Remove
        </Button>
      </div>
      <QuotaLine ariaLabel={`Tokens of ${t.team} on ${m.name} this month`} use={{ used: t.used, limit: t.limit }} />
      <LimitField
        key={t.limit}
        label={`Limit of ${t.team} on ${m.name}`}
        saveLabel={`Save the limit of ${t.team} on ${m.name}`}
        hint="Empty: no limit for this team."
        limit={t.limit}
        path={paths.apiAdminModelTeamLimit(m.id, t.teamId)}
        onSaved={onChanged}
      />
      {remove.isError && <ErrorBox error={remove.error} />}
    </div>
  );
}

// AddTeamLimit gives a team with no limit on the entry one. The teams are
// read when the list is opened.
function AddTeamLimit({ m, onAdded }: { m: ModelAdminView; onAdded: () => void }) {
  const write = useWrite();
  const teams = useQuery({ queryKey: ["teams"], queryFn: () => api.get<TeamView[]>("/api/teams") });
  const [team, setTeam] = useState("");
  const [text, setText] = useState("");
  const [problem, setProblem] = useState("");
  const add = useMutation({
    mutationFn: ({ id, n }: { id: number; n: number }) => write<void>("PUT", paths.apiAdminModelTeamLimit(m.id, id), { monthlyTokens: n }),
    onSuccess: () => {
      setTeam("");
      setText("");
      onAdded();
    },
  });
  if (teams.isPending) return <Loading />;
  if (teams.isError) return <ErrorBox error={teams.error} />;
  const limited = new Set(m.teamLimits.map((t) => t.teamId));
  const free = teams.data.filter((t) => !limited.has(t.id));
  if (free.length === 0) {
    return <div className={hint}>{teams.data.length === 0 ? "No team yet." : "Every team has a limit on it."}</div>;
  }
  const submit = () => {
    const n = parseQuota(text);
    if (team === "" || n === null || n === undefined) {
      setProblem(addLimitProblem);
      return;
    }
    setProblem("");
    add.mutate({ id: Number(team), n });
  };
  return (
    <div className="grid gap-2">
      <div className="flex flex-wrap items-center gap-2">
        <NativeSelect className="w-48" aria-label={`Team to limit on ${m.name}`} value={team} onChange={(e) => setTeam(e.target.value)}>
          <option value="">Pick a team</option>
          {free.map((t) => (
            <option key={t.id} value={String(t.id)}>
              {t.name}
            </option>
          ))}
        </NativeSelect>
        <Input
          className="w-48"
          type="text"
          inputMode="numeric"
          aria-label={`New team limit on ${m.name}`}
          placeholder="tokens per month"
          value={text}
          onChange={(e) => setText(e.target.value)}
        />
        <Button type="button" variant="outline" size="sm" aria-label={`Add a team limit on ${m.name}`} disabled={add.isPending} onClick={submit}>
          Add
        </Button>
      </div>
      {problem && <div className="text-bad text-sm">{problem}</div>}
      {add.isError && <ErrorBox error={add.error} />}
    </div>
  );
}

// GlobalChain is the chain reviews use when neither a repository nor its
// owner team has one: the config file's entries in front, then the
// dashboard's in the order the admin puts them. Position 1 is the default.
function GlobalChain() {
  const client = useQueryClient();
  const write = useWrite();
  const chain = useQuery({ queryKey: chainKey, queryFn: () => api.get<ChainView>("/api/admin/chain") });
  const [open, setOpen] = useState(false);
  const [note, setNote] = useState("");
  const changed = () => {
    client.invalidateQueries({ queryKey: modelsKey });
    client.invalidateQueries({ queryKey: ["repo"] });
    client.invalidateQueries({ queryKey: ["repo-chain"] });
  };
  const save = useMutation({
    mutationFn: (entries: ChainRef[]) => write<ChainView>("PUT", "/api/admin/chain", { entries }),
    onMutate: () => setNote(""),
    onSuccess: (next) => {
      client.setQueryData(chainKey, next);
      changed();
      setNote("Saved.");
    },
  });
  const clear = useMutation({
    mutationFn: (e: ChainEntryView) => write<void>("DELETE", paths.apiAdminModelMark(e.id)),
    onSuccess: () => {
      client.invalidateQueries({ queryKey: chainKey });
      client.invalidateQueries({ queryKey: ["repo-reviewers"] });
      changed();
    },
  });
  return (
    <Card role="region" className={foldCard(open)} aria-label="Global chain">
      <FoldHead title="Global chain" summary={chainSummary(chain.data)} open={open} onToggle={() => setOpen(!open)} />
      <div hidden={!open}>
        <div className={cardText}>
          Reviews try the entries in this order. When the provider says an entry is out of quota or credit, or rejects its key, the
          review starts again on the next one. The first entry is the default.
        </div>
        {chain.isPending && <Loading />}
        {chain.isError && <ErrorBox error={chain.error} />}
        {chain.isSuccess && (
          <ChainEditor
            // A new answer from the server starts the editor again from it.
            key={JSON.stringify(chain.data)}
            value={chain.data}
            choices={chain.data.choices}
            readOnly={chain.data.readOnly}
            allowEmpty
            busy={save.isPending}
            note={note}
            onSave={(entries) => save.mutate(entries)}
            onClear={(e) => clear.mutate(e)}
          />
        )}
        {save.isError && <ErrorBox error={save.error} />}
        {clear.isError && <ErrorBox error={clear.error} />}
      </div>
    </Card>
  );
}

// chainSummary is the folded chain card's line: how many entries, and the
// first, which reviews try first.
function chainSummary(c: ChainView | undefined) {
  if (!c) return "";
  if (c.entries.length === 0) return "No entries";
  return `${countText(c.entries.length, "entry", "entries")} · first ${c.entries[0].name}`;
}
