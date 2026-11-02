import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useCallback, useEffect, useRef, useState, type ReactNode } from "react";

import {
  ApiError,
  api,
  isStatus,
  paths,
  type ChecklistItem,
  type ChecklistSection,
  type ChecklistState,
  type NoiseFile,
  type ReadinessView,
  type ScanView,
} from "../api";
import { useWrite } from "../layout/write";
import { ErrorBox, Loading, Pill, bytesText, type Tone } from "../ui";

// How often the tab asks again while a scan is queued or running.
export const pollMs = 5_000;

// The noise list shows this many paths; the API already stops there.
const noiseShown = 50;

const stateLabel: Record<ChecklistState, [Tone, string]> = {
  passed: ["ok", "Passed"],
  warning: ["warn", "Warning"],
  missing: ["bad", "Missing"],
  // Couldn't check is never green.
  unknown: ["grey", "Couldn't check"],
};

const sectionTitle: Record<string, string> = {
  context: "Project context",
  process: "Ownership and process",
  skills: "Skill fit",
  size: "Size and noise",
};

const itemTitle: Record<string, string> = {
  agent_file: "Agent context file (AGENTS.md or CLAUDE.md)",
  readme: "README",
  contributing: "Contributing guide",
  instructions: "Review instructions (.pergent/instructions.md)",
  codeowners: "CODEOWNERS",
  approvals: "Approvals needed to merge",
  protected: "Default branch protected",
  required_checks: "Required checks before merge",
  skill_match: "Skills that match files",
  skills_unmatched: "Assigned skills that match nothing",
  languages: "Languages no skill covers",
  noise: "Generated, vendored and very large files",
  budget: "Skills plus instructions size",
};

// Readiness is a repository's readiness scan: what the last scan found, with
// why it matters and the fix, and Scan now for maintainers and admins. Every
// string in it comes from a scan and is shown as text.
export default function Readiness({ instance, project }: { instance: string; project: string }) {
  const client = useQueryClient();
  const write = useWrite();
  const key = ["readiness", instance, project];
  // The tab asks again every 5 seconds while a scan is active, and not at all
  // otherwise. Leaving the tab ends the query's observer, so it stops there too.
  const readiness = useQuery({
    queryKey: key,
    queryFn: () => api.get<ReadinessView>(paths.apiRepoReadiness(instance, project)),
    refetchInterval: (q) => (q.state.data?.active ? pollMs : false),
  });
  const [note, setNote] = useState("");

  // What a scan or a save changes elsewhere: the setup checks and the overview's
  // Needs attention.
  const refreshAround = useCallback(() => {
    client.invalidateQueries({ queryKey: ["repo", instance] });
    client.invalidateQueries({ queryKey: ["repositories"] });
    client.invalidateQueries({ queryKey: ["overview"] });
  }, [client, instance]);

  // A scan that just ended may have raised or cleared Needs attention items.
  const wasActive = useRef(false);
  const isActive = readiness.data ? readiness.data.active !== null : undefined;
  useEffect(() => {
    if (isActive === undefined) return;
    if (wasActive.current && !isActive) refreshAround();
    wasActive.current = isActive;
  }, [isActive, refreshAround]);

  const scanNow = useMutation({
    mutationFn: () => write<{ scan: ScanView }>("POST", paths.apiRepoReadinessScan(instance, project), {}),
    onMutate: () => setNote(""),
    onSuccess: () => client.invalidateQueries({ queryKey: key }),
    // 409: a scan is already queued or running; the tab shows it next.
    onError: (err) => {
      if (isStatus(err, 409)) client.invalidateQueries({ queryKey: key });
    },
  });

  const apply = useMutation({
    mutationFn: (skills: string[]) => write("PUT", paths.apiRepoSettings(instance, project), { skills }),
    onMutate: () => setNote(""),
    onSuccess: () => {
      client.invalidateQueries({ queryKey: key });
      client.invalidateQueries({ queryKey: ["inputs", instance, project] });
      refreshAround();
      setNote("Saved.");
    },
  });

  if (readiness.isPending) return <Loading />;
  if (readiness.isError) return <ErrorBox error={readiness.error} />;
  const v = readiness.data;
  const scanning = v.active !== null;

  return (
    <>
      <section className="card pad stack" aria-label="Scan">
        <div className="row wrap">
          <div className="grow">
            {scanning && <div role="status">Scanning…</div>}
            {v.latest ? <TopLine scan={v.latest} /> : !scanning && <div>Not scanned yet</div>}
          </div>
          {v.canScan && (
            <button type="button" className="btn small" disabled={scanning || scanNow.isPending} onClick={() => scanNow.mutate()}>
              Scan now
            </button>
          )}
        </div>
        <ScanError error={scanNow.isError ? scanNow.error : null} />
        {note && <div className="note">{note}</div>}
      </section>
      {apply.isError && <ErrorBox error={apply.error} />}
      {v.checklist === null ? (
        <div className="card empty">{scanning ? "The first scan is running." : "Scan this repository to see what it needs."}</div>
      ) : (
        <Checklist view={v} checklist={v.checklist} saving={apply.isPending} onApply={(skills) => apply.mutate(skills)} />
      )}
    </>
  );
}

// Checklist is one card per check. The suggested skills sit with Skill fit.
function Checklist({
  view,
  checklist,
  saving,
  onApply,
}: {
  view: ReadinessView;
  checklist: NonNullable<ReadinessView["checklist"]>;
  saving: boolean;
  onApply: (skills: string[]) => void;
}) {
  const suggestions = <Suggestions view={view} suggested={checklist.suggested} saving={saving} onApply={onApply} />;
  const hasSkills = checklist.sections.some((s) => s.key === "skills");
  return (
    <>
      {checklist.sections.map((s) => (
        <Section key={s.key} section={s}>
          {s.key === "skills" && suggestions}
        </Section>
      ))}
      {!hasSkills && checklist.suggested.length > 0 && <section className="card">{suggestions}</section>}
    </>
  );
}

// scannedAgo is how long before now t was, in whole days, hours or minutes.
function scannedAgo(t: string, now = Date.now()): string {
  const min = Math.floor((now - new Date(t).getTime()) / 60_000);
  if (Number.isNaN(min) || min < 1) return "less than a minute";
  const unit = (n: number, one: string) => `${n} ${one}${n === 1 ? "" : "s"}`;
  if (min < 60) return unit(min, "minute");
  const h = Math.floor(min / 60);
  if (h < 24) return unit(h, "hour");
  return unit(Math.floor(h / 24), "day");
}

// TopLine is "Scanned 3 days ago, by alice (gitlab main), at commit abcdef1".
function TopLine({ scan }: { scan: ScanView }) {
  const who =
    scan.startedBy === "switch-on"
      ? "when the repository was switched on"
      : scan.startedBy === "upgrade"
        ? "after the upgrade"
        : `by ${scan.startedBy}`;
  const at = scan.commit ? `, at commit ${scan.commit.slice(0, 7)}` : "";
  return <div className="sub">{`Scanned ${scannedAgo(scan.finishedAt ?? scan.queuedAt)} ago, ${who}${at}`}</div>;
}

// ScanError says why Scan now didn't queue a scan. The wait comes from
// waitSeconds, rounded up to whole minutes.
function ScanError({ error }: { error: unknown }) {
  if (error === null) return null;
  if (isStatus(error, 409)) return <div role="alert" className="error">A scan is already running.</div>;
  if (isStatus(error, 429)) {
    const wait = (error as ApiError).body as { waitSeconds?: unknown } | undefined;
    if (typeof wait?.waitSeconds === "number" && wait.waitSeconds > 0) {
      const minutes = Math.ceil(wait.waitSeconds / 60);
      return (
        <div role="alert" className="error">
          Try again in {minutes} {minutes === 1 ? "minute" : "minutes"}.
        </div>
      );
    }
  }
  return <ErrorBox error={error} />;
}

function Section({ section, children }: { section: ChecklistSection; children?: ReactNode }) {
  const title = sectionTitle[section.key] ?? section.key;
  return (
    <section className="card" aria-label={title}>
      <h2>{title}</h2>
      {section.items.map((it) => (
        <Item key={it.key} item={it} />
      ))}
      {children}
    </section>
  );
}

function Item({ item }: { item: ChecklistItem }) {
  const [tone, label] = stateLabel[item.state] ?? ["grey", "Couldn't check"];
  const noise = item.noise ?? [];
  return (
    <div className="row">
      <Pill tone={tone}>{label}</Pill>
      <div className="grow">
        <div className="title">{itemTitle[item.key] ?? item.key}</div>
        {item.reason && <div className="why prose">{item.reason}</div>}
        {item.why && <div className="why prose">{item.why}</div>}
        {item.list && item.list.length > 0 && (
          <ul className="why">
            {item.list.map((l, i) => (
              <li key={i} className="prose">
                {l}
              </li>
            ))}
          </ul>
        )}
        {noise.length > 0 && <NoiseList noise={noise} more={item.more ?? 0} />}
        {item.fix && <pre className="fix">{item.fix}</pre>}
      </div>
    </div>
  );
}

// NoiseList is the files a review reads that look like noise: at most 50, and
// the count of the rest (what the API left out and what this page cuts).
function NoiseList({ noise, more }: { noise: NoiseFile[]; more: number }) {
  const shown = noise.slice(0, noiseShown);
  const rest = more + (noise.length - shown.length);
  return (
    <ul className="why">
      {shown.map((n, i) => (
        <li key={i} className="prose">
          <span className="mono">{n.path}</span> · {n.why} · {bytesText(n.size)}
          {n.skills && n.skills.length > 0 && <> · skills: {n.skills.join(", ")}</>}
        </li>
      ))}
      {rest > 0 && <li>and {rest} more</li>}
    </ul>
  );
}

// The skills line a config-file repository pastes.
const skillsLine = (names: string[]) => `skills: [${names.join(", ")}]`;

// Suggestions are the library skills that fit the files and are not on. A
// maintainer or admin ticks some and saves them with the skills already on; a
// config-file repository gets the line to paste instead of the button.
function Suggestions({
  view,
  suggested,
  saving,
  onApply,
}: {
  view: ReadinessView;
  suggested: string[];
  saving: boolean;
  onApply: (skills: string[]) => void;
}) {
  const [ticked, setTicked] = useState<string[]>([]);
  if (suggested.length === 0) return null;
  const choose = view.configRepo || view.canScan;
  // Keep the order of the suggestions, and a skill already on once.
  const list = [...view.applyBase, ...suggested.filter((s) => ticked.includes(s) && !view.applyBase.includes(s))];
  const save = () => {
    if (view.followsDefaults && !window.confirm("This repository will stop following the default skill list.")) return;
    onApply(list);
  };
  return (
    <div className="pad stack" aria-label="Suggested skills">
      <h3 className="group">Suggested skills</h3>
      {suggested.map((s) =>
        choose ? (
          <label className="check" key={s}>
            <input
              type="checkbox"
              checked={ticked.includes(s)}
              onChange={(e) => setTicked(e.target.checked ? [...ticked, s] : ticked.filter((t) => t !== s))}
            />
            {s}
          </label>
        ) : (
          <div key={s}>{s}</div>
        ),
      )}
      {view.configRepo ? (
        <>
          <div className="meta">This repository is set up in the config file. Paste this line there:</div>
          <pre className="fix">{skillsLine(list)}</pre>
        </>
      ) : (
        view.canScan && (
          <div>
            <button type="button" className="btn primary small" disabled={ticked.length === 0 || saving} onClick={save}>
              Use these skills
            </button>
          </div>
        )
      )}
    </div>
  );
}
