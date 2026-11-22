import { useId } from "react";

import type { CIJob, CIJobRead, CIJobs } from "./api";
import { Switch } from "./ui";

// The most jobs a repository chooses; the server refuses an eleventh.
export const maxCIJobs = 10;

export const providerWarning = "CI output goes to the model provider. Secrets are removed first, but only the shapes pergent.sh knows.";
export const unknownNameWarning = "Not seen in the last 10 runs. Check the name.";

// tidyCIJobs is the jobs as they are sent: names and paths trimmed, and an
// empty artifact left out. The editor keeps what is typed as it is.
export function tidyCIJobs(c: CIJobs): CIJobs {
  return {
    wait: c.wait.trim(),
    jobs: c.jobs.map((j) => {
      const job: CIJob = { name: j.name.trim(), read: j.read, blocksApproval: j.blocksApproval };
      if (j.read === "report" && j.report) {
        const artifact = (j.report.artifact ?? "").trim();
        job.report = artifact === "" ? { path: j.report.path.trim() } : { artifact, path: j.report.path.trim() };
      }
      return job;
    }),
  };
}

// CIJobsEditor edits the CI jobs a review waits for and reads: how long to
// wait, then one row per job (name, what to read, a report's place, whether it
// blocks approval, Remove). names are the job or check names seen in the last
// runs: they are suggested, any text is allowed, and a name that is not among
// them gets a warning (null: they could not be read, so no warning). Every
// change calls onChange with the whole value.
export default function CIJobsEditor({
  value,
  onChange,
  names,
  github,
  disabled,
}: {
  value: CIJobs;
  onChange: (v: CIJobs) => void;
  names: string[] | null;
  github: boolean;
  disabled?: boolean;
}) {
  const listId = useId();
  const change = (i: number, edit: Partial<CIJob>) =>
    onChange({ ...value, jobs: value.jobs.map((j, k) => (k === i ? { ...j, ...edit } : j)) });
  const setRead = (i: number, read: CIJobRead) => {
    const job = value.jobs[i];
    const next: CIJob = { name: job.name, read, blocksApproval: job.blocksApproval };
    if (read === "report") next.report = job.report ?? { path: "" };
    onChange({ ...value, jobs: value.jobs.map((j, k) => (k === i ? next : j)) });
  };
  const setReport = (i: number, edit: { artifact?: string; path?: string }) => {
    const r = { ...value.jobs[i].report, path: value.jobs[i].report?.path ?? "", ...edit };
    if (r.artifact === "") delete r.artifact;
    change(i, { report: r });
  };
  const full = value.jobs.length >= maxCIJobs;

  return (
    <div>
      <div className="set">
        <div>
          <div className="name">Wait for CI</div>
          <div className="help">
            A review waits this long for the jobs below to end, from 5m to 2h, for example 30m. A blocking job that has not passed
            holds approval back.
          </div>
        </div>
        <div>
          <input
            type="text"
            aria-label="How long to wait for CI"
            spellCheck={false}
            value={value.wait}
            disabled={disabled}
            onChange={(e) => onChange({ ...value, wait: e.target.value })}
          />
        </div>
      </div>

      <datalist id={listId}>
        {(names ?? []).map((n) => (
          <option key={n} value={n} />
        ))}
      </datalist>
      <div role="list" aria-label="CI jobs">
        {value.jobs.map((j, i) => {
          const unknown = names !== null && j.name.trim() !== "" && !names.includes(j.name.trim());
          return (
            <div className="link-row ci-job" role="listitem" key={i}>
              <div className="link-head">
                <input
                  type="text"
                  role="combobox"
                  className="grow"
                  aria-label="Job name"
                  placeholder="Job name"
                  list={listId}
                  spellCheck={false}
                  value={j.name}
                  disabled={disabled}
                  onChange={(e) => change(i, { name: e.target.value })}
                />
                <select aria-label="Read" value={j.read} disabled={disabled} onChange={(e) => setRead(i, e.target.value as CIJobRead)}>
                  <option value="status">status only</option>
                  <option value="log">status and log</option>
                  <option value="report">status and report</option>
                </select>
                <label className="link-block">
                  <Switch label="Blocks approval" checked={j.blocksApproval} disabled={disabled} onChange={(on) => change(i, { blocksApproval: on })} />
                  <span>Blocks approval</span>
                </label>
                <button
                  type="button"
                  className="btn small"
                  aria-label={`Remove ${j.name.trim() || "job"}`}
                  disabled={disabled}
                  onClick={() => onChange({ ...value, jobs: value.jobs.filter((_, k) => k !== i) })}
                >
                  Remove
                </button>
              </div>
              {j.read === "report" && (
                <div className="link-head">
                  {github && (
                    <input
                      type="text"
                      className="grow"
                      aria-label="Report artifact"
                      placeholder="Workflow artifact name"
                      spellCheck={false}
                      value={j.report?.artifact ?? ""}
                      disabled={disabled}
                      onChange={(e) => setReport(i, { artifact: e.target.value })}
                    />
                  )}
                  <input
                    type="text"
                    className="grow"
                    aria-label="Report path"
                    placeholder="Path of the report in the job's artifacts"
                    spellCheck={false}
                    value={j.report?.path ?? ""}
                    disabled={disabled}
                    onChange={(e) => setReport(i, { path: e.target.value })}
                  />
                </div>
              )}
              {unknown && <div className="meta warn-text">{unknownNameWarning}</div>}
              {j.read !== "status" && <div className="meta warn-text">{providerWarning}</div>}
            </div>
          );
        })}
      </div>
      {value.jobs.length === 0 && <div className="pad meta">No CI job yet.</div>}
      <div className="pad">
        {full ? (
          <div className="meta">A repository has at most {maxCIJobs} jobs. Remove one to add another.</div>
        ) : null}
        <button
          type="button"
          className="btn small"
          disabled={disabled || full}
          onClick={() => onChange({ ...value, jobs: [...value.jobs, { name: "", read: "status", blocksApproval: false }] })}
        >
          Add a job
        </button>
      </div>
    </div>
  );
}
