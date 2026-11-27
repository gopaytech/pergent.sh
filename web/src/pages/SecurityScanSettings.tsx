import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState, type ReactNode } from "react";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";

import { api, paths, type ChainRef, type ChainView, type SecuritySettingsRequest, type SecuritySettingsView } from "../api";
import ChainEditor from "../ChainEditor";
import { FoldCard } from "../Fold";
import { useWrite } from "../layout/write";
import { useClearMark } from "../ReviewersCard";
import { ErrorBox, Loading, Switch } from "../ui";

const row = "grid grid-cols-1 items-center gap-4 border-b px-4 py-3.5 last:border-b-0 sm:grid-cols-[1fr_280px]";

// The ranges the API accepts.
const mainRange = { min: 5, max: 1000 };
const subRange = { min: 5, max: 300 };
const timeoutRange = { min: 10, max: 360 };

// whole reads a typed number: empty is null (the default), a whole number in
// range is that number, anything else is undefined.
function whole(text: string, min: number, max: number): number | null | undefined {
  const t = text.trim();
  if (t === "") return null;
  if (!/^\d+$/.test(t)) return undefined;
  const n = Number(t);
  return n >= min && n <= max ? n : undefined;
}

// longText is a number of seconds as the default timeout reads: "3 h".
function longText(seconds: number): string {
  if (seconds % 3600 === 0) return `${seconds / 3600} h`;
  if (seconds % 60 === 0) return `${seconds / 60} min`;
  return `${seconds} s`;
}

const minutesText = (seconds: number | null) => (seconds === null ? "" : String(Math.round(seconds / 60)));

const settingsKey = (instance: string, project: string) => ["security-settings", instance, project];
const chainKey = (instance: string, project: string) => ["security-chain", instance, project];

// SecurityScanSettings is a repository's Security scan card: the scan's model
// chain (empty: the repository's own chain), its step limits and timeout, and
// whether reviews get the open findings. Everyone who sees the repository
// sees it; the server says who edits (canEdit, and readOnly on the chain). On
// a paused repository nothing is edited, and that is not an error.
export default function SecurityScanSettings({ instance, project, paused }: { instance: string; project: string; paused: boolean }) {
  const settings = useQuery({
    queryKey: settingsKey(instance, project),
    queryFn: () => api.get<SecuritySettingsView>(paths.apiRepoSecuritySettings(instance, project)),
  });
  // Kept here: a saved answer remounts the form, which would lose it.
  const [note, setNote] = useState("");
  return (
    <FoldCard label="Security scan" title="Security scan" summary="scan model, limits, findings in reviews">
      <ScanChain instance={instance} project={project} paused={paused} />
      {settings.isPending && <Loading />}
      {settings.isError && (
        <div className="px-4 py-3">
          <ErrorBox error={settings.error} onRetry={() => void settings.refetch()} />
        </div>
      )}
      {settings.isSuccess && (
        // A new answer from the server starts the form again.
        <Limits key={JSON.stringify(settings.data)} instance={instance} project={project} view={settings.data} paused={paused} note={note} setNote={setNote} />
      )}
    </FoldCard>
  );
}

function Row({ name, help, children }: { name: string; help?: ReactNode; children: ReactNode }) {
  return (
    <div className={row}>
      <div>
        <div className="text-sm font-semibold">{name}</div>
        {help && <div className="text-muted-foreground mt-0.5 text-sm">{help}</div>}
      </div>
      <div className="grid gap-1.5">{children}</div>
    </div>
  );
}

function Limits({
  instance,
  project,
  view,
  paused,
  note,
  setNote,
}: {
  instance: string;
  project: string;
  view: SecuritySettingsView;
  paused: boolean;
  note: string;
  setNote: (note: string) => void;
}) {
  const client = useQueryClient();
  const write = useWrite();
  const editable = view.canEdit && !paused;
  const [main, setMain] = useState(view.maxSteps === null ? "" : String(view.maxSteps));
  const [sub, setSub] = useState(view.subagentMaxSteps === null ? "" : String(view.subagentMaxSteps));
  const [minutes, setMinutes] = useState(minutesText(view.timeoutSeconds));
  const [findings, setFindings] = useState(view.findingsInReviews);
  const [problem, setProblem] = useState("");
  const save = useMutation({
    mutationFn: (body: SecuritySettingsRequest) => write<SecuritySettingsView>("PUT", paths.apiRepoSecuritySettings(instance, project), body),
    onMutate: () => setNote(""),
    onSuccess: (next) => {
      client.setQueryData(settingsKey(instance, project), next);
      // The Security tab shows the limits a scan would use.
      void client.invalidateQueries({ queryKey: ["security", instance, project] });
      setNote("Saved.");
    },
  });
  const submit = () => {
    save.reset();
    const m = whole(main, mainRange.min, mainRange.max);
    const s = whole(sub, subRange.min, subRange.max);
    // An untouched timeout goes back as it came, to the second.
    const t = minutes.trim() === minutesText(view.timeoutSeconds) ? "same" : whole(minutes, timeoutRange.min, timeoutRange.max);
    if (m === undefined) return setProblem(`Max steps for the main agent: a whole number from ${mainRange.min} to ${mainRange.max}, or empty.`);
    if (s === undefined) return setProblem(`Max steps for each sub-agent: a whole number from ${subRange.min} to ${subRange.max}, or empty.`);
    if (t === undefined) return setProblem(`Scan timeout: a whole number of minutes from ${timeoutRange.min} to ${timeoutRange.max}, or empty.`);
    setProblem("");
    // The API replaces all 4 fields: a field left out would reset.
    save.mutate({
      maxSteps: m,
      subagentMaxSteps: s,
      timeoutSeconds: t === "same" ? view.timeoutSeconds : t === null ? null : t * 60,
      findingsInReviews: findings,
    });
  };
  const d = view.defaults;
  const findingsHelp = "Reviews get the open findings in the files a change touches, as data. They never block a merge.";
  if (!editable) {
    return (
      <>
        <Row name="Max steps, main agent">{view.maxSteps ?? `from the skill (${d.maxSteps})`}</Row>
        <Row name="Max steps, each sub-agent">{view.subagentMaxSteps ?? `from the skill (${d.subagentMaxSteps})`}</Row>
        <Row name="Scan timeout">{view.timeoutSeconds === null ? longText(d.timeoutSeconds) : longText(view.timeoutSeconds)}</Row>
        <Row name="Use security findings in reviews" help={findingsHelp}>
          <span>{view.findingsInReviews ? "On" : "Off"}</span>
        </Row>
        {paused && <div className="text-muted-foreground border-t px-4 py-3 text-sm">This repository is paused, so its scan settings can't be changed.</div>}
      </>
    );
  }
  return (
    <>
      <Row name="Max steps, main agent" help={`${mainRange.min} to ${mainRange.max}. Empty: from the skill.`}>
        <Input
          type="text"
          inputMode="numeric"
          aria-label="Max steps, main agent"
          placeholder={`from the skill (${d.maxSteps})`}
          value={main}
          onChange={(e) => setMain(e.target.value)}
        />
      </Row>
      <Row name="Max steps, each sub-agent" help={`${subRange.min} to ${subRange.max}. Empty: from the skill.`}>
        <Input
          type="text"
          inputMode="numeric"
          aria-label="Max steps, each sub-agent"
          placeholder={`from the skill (${d.subagentMaxSteps})`}
          value={sub}
          onChange={(e) => setSub(e.target.value)}
        />
      </Row>
      <Row name="Scan timeout" help={`In minutes, ${timeoutRange.min} to ${timeoutRange.max}. Empty: ${longText(d.timeoutSeconds)}.`}>
        <Input
          type="text"
          inputMode="numeric"
          aria-label="Scan timeout (minutes)"
          placeholder={longText(d.timeoutSeconds)}
          value={minutes}
          onChange={(e) => setMinutes(e.target.value)}
        />
      </Row>
      <Row name="Use security findings in reviews" help={findingsHelp}>
        <Switch label="Use security findings in reviews" checked={findings} onChange={setFindings} />
      </Row>
      <div className="bg-muted/40 flex items-center gap-3 border-t px-4 py-3">
        <Button type="button" size="sm" aria-label="Save scan settings" disabled={save.isPending} onClick={submit}>
          Save
        </Button>
        {note && (
          <span className="text-ok text-sm" role="status">
            {note}
          </span>
        )}
      </div>
      {problem && <div className="text-bad px-4 pb-3 text-sm">{problem}</div>}
      {save.isError && (
        <div className="px-4 pb-3">
          <ErrorBox error={save.error} />
        </div>
      )}
    </>
  );
}

// ScanChain is the scan's model chain in effect. With none of its own it is
// the repository's chain; Edit gives the scan its own, starting from that,
// and Inherit drops it again. The write rule is the repository chain's.
function ScanChain({ instance, project, paused }: { instance: string; project: string; paused: boolean }) {
  const client = useQueryClient();
  const write = useWrite();
  const key = chainKey(instance, project);
  const chain = useQuery({ queryKey: key, queryFn: () => api.get<ChainView>(paths.apiRepoSecurityChain(instance, project)) });
  const [editing, setEditing] = useState(false);
  const [note, setNote] = useState("");
  const done = (next: ChainView, message: string) => {
    client.setQueryData(key, next);
    // The Security tab names the model a scan would use.
    void client.invalidateQueries({ queryKey: ["security", instance, project] });
    setEditing(false);
    setNote(message);
  };
  const save = useMutation({
    mutationFn: (entries: ChainRef[]) => write<ChainView>("PUT", paths.apiRepoSecurityChain(instance, project), { entries }),
    onMutate: () => setNote(""),
    onSuccess: (next) => done(next, "Saved."),
  });
  const reset = useMutation({
    mutationFn: () => write<ChainView>("POST", paths.apiRepoSecurityChainReset(instance, project), {}),
    onMutate: () => setNote(""),
    onSuccess: (next) => done(next, "The scan uses the repository's chain again."),
  });
  const clear = useClearMark();
  const busy = save.isPending || reset.isPending;
  const c = chain.data;
  const can = c !== undefined && !c.readOnly && !paused;
  const own = c?.source === "repository";
  const shown = can && editing;
  return (
    <div role="group" aria-label="Scan model" className="border-b">
      <div className="flex items-center gap-2 px-4 py-3 text-sm">
        <b className="min-w-0 grow font-semibold">Scan model</b>
        {can && !editing && (
          <Button type="button" variant="outline" size="sm" aria-label="Edit scan model" disabled={busy} onClick={() => { setNote(""); setEditing(true); }}>
            Edit
          </Button>
        )}
        {can && own && !editing && (
          <Button
            type="button"
            variant="outline"
            size="sm"
            disabled={busy}
            onClick={() => {
              if (window.confirm("Drop the scan's own chain? Scans then use the repository's model chain.")) reset.mutate();
            }}
          >
            Inherit the repository's chain
          </Button>
        )}
      </div>
      {chain.isPending && <Loading />}
      {chain.isError && (
        <div className="px-4 py-3">
          <ErrorBox error={chain.error} />
        </div>
      )}
      {c && (
        <>
          <div className="text-muted-foreground px-4 pb-3 text-sm">
            {own ? "This repository has its own scan chain." : "The repository's model chain"}
          </div>
          <ChainEditor
            // A new answer from the server, or the start of an edit, starts the editor again.
            key={`${JSON.stringify(c)}:${shown}`}
            label="Scan model chain"
            value={c}
            choices={shown ? c.choices : []}
            readOnly={!shown}
            busy={busy}
            onSave={(entries) => save.mutate(entries)}
            onCancel={() => setEditing(false)}
            onClear={(e) => clear.mutate(e)}
          />
        </>
      )}
      {note && (
        <div className="text-ok border-t px-4 py-3 text-sm" role="status">
          {note}
        </div>
      )}
      {save.isError && (
        <div className="px-4 py-3">
          <ErrorBox error={save.error} />
        </div>
      )}
      {reset.isError && (
        <div className="px-4 py-3">
          <ErrorBox error={reset.error} />
        </div>
      )}
    </div>
  );
}
