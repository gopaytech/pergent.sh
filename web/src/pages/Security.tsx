import { useMutation, useQuery, useQueryClient, type UseMutationResult } from "@tanstack/react-query";
import { useState } from "react";
import { useSearchParams } from "react-router";

import { Button, buttonVariants } from "@/components/ui/button";
import { Card, CardHeader, CardTitle } from "@/components/ui/card";
import { Dialog, DialogClose, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle, DialogTrigger } from "@/components/ui/dialog";

import { api, paths, type SecurityScanView, type SecurityStartView, type SecurityView } from "../api";
import { useWrite } from "../layout/write";
import { ErrorBox, Loading, Pill, count, duration, type Tone } from "../ui";
import SecurityFinding from "./SecurityFinding";
import SecurityFindings from "./SecurityFindings";

// How often the tab asks again while a scan is queued or running.
export const securityPollMs = 10_000;

// cardClass is a card; headClass is its title bar.
const cardClass = "mb-4 gap-0 overflow-hidden py-0";
const headClass = "border-b py-3 [.border-b]:pb-3";
const lineClass = "border-b px-4 py-3 text-sm last:border-b-0";

const stateTone: Record<SecurityScanView["state"], [Tone, string]> = {
  queued: ["grey", "Queued"],
  running: ["warn", "Running"],
  complete: ["ok", "Complete"],
  partial: ["warn", "Partial"],
  failed: ["bad", "Failed"],
};

// Security is the Security tab of a repository: the last scan, the open
// findings by severity, Start scan and Cancel. Every text of a scan comes
// from the model or the platform and is shown as text. paused: no scan can
// start, and the page says so as a note, not an error. The finding and scan
// pages are this tab with ?finding= or ?scan= in the address.
export default function Security({ instance, project, paused }: { instance: string; project: string; paused: boolean }) {
  const [search] = useSearchParams();
  // Task 5 shows the scan page in place of the summary.
  void search.get("scan");
  const finding = Number(search.get("finding"));
  const key = ["security", instance, project];
  // The tab asks again every 10 seconds while a scan is queued or running, and
  // not at all otherwise.
  const security = useQuery({
    queryKey: key,
    queryFn: () => api.get<SecurityView>(paths.apiRepoSecurity(instance, project)),
    refetchInterval: (q) => (q.state.data?.running ? securityPollMs : false),
  });
  // A finding's page takes the place of the tab's summary and list.
  if (search.has("finding") && Number.isInteger(finding) && finding > 0) {
    return <SecurityFinding instance={instance} project={project} id={finding} paused={paused} />;
  }
  if (security.isPending) return <Loading />;
  if (security.isError) return <ErrorBox error={security.error} onRetry={() => void security.refetch()} />;
  const v = security.data;
  return <Summary v={v} instance={instance} project={project} paused={paused} queryKey={key} />;
}

function Summary({ v, instance, project, paused, queryKey }: { v: SecurityView; instance: string; project: string; paused: boolean; queryKey: unknown[] }) {
  const client = useQueryClient();
  const write = useWrite();
  const refresh = () => client.invalidateQueries({ queryKey });
  const [open, setOpen] = useState(false);
  // The start lives here, not in the box: a 409 usually means a scan is
  // active, the refresh then drops the box, and the error must stay readable.
  const start = useMutation({
    mutationFn: () => write("POST", paths.apiRepoSecurityScans(instance, project), {}),
    onSuccess: () => setOpen(false),
    onSettled: refresh,
  });
  const cancel = useMutation({
    mutationFn: (id: string) => write("POST", paths.apiRepoSecurityScanCancel(instance, project, id), {}),
    // A 409 means the scan ended meanwhile: the tab shows its end next.
    onSettled: refresh,
  });
  const ask = (scan: SecurityScanView) => {
    if (window.confirm(`Cancel scan ${scan.number}? Its findings so far are kept.`)) cancel.mutate(scan.id);
  };
  const { running, last, counts } = v;
  return (
    <>
      <Card role="region" aria-label="Last scan" className={cardClass}>
        <CardHeader className={headClass}>
          <CardTitle>Last scan</CardTitle>
        </CardHeader>
        {last ? <LastScan scan={last} /> : <div className={lineClass + " text-muted-foreground"}>No scan yet.</div>}
        <div className={lineClass} aria-label="Open findings">
          Critical {counts.critical} · High {counts.high} · Medium {counts.medium} · Low {counts.low} · Needs validation {counts.needsValidation}
        </div>
        <div className={lineClass + " flex flex-wrap items-center gap-3"}>
          {v.canManage && (
            <>
              {running ? (
                <Button type="button" variant="outline" size="sm" disabled>
                  Running
                </Button>
              ) : (
                <StartScan v={v} start={start} open={open} setOpen={setOpen} />
              )}
              {running && (
                <Button type="button" variant="outline" size="sm" disabled={cancel.isPending || running.cancelRequested} onClick={() => ask(running)}>
                  Cancel
                </Button>
              )}
            </>
          )}
          {running && (
            <span role="status" className="text-muted-foreground">
              {running.state === "queued" ? `Scan ${running.number} is queued.` : `Scan ${running.number} is running.`}
              {running.cancelRequested && " Cancelling."}
            </span>
          )}
          {!running && v.canManage && v.startOff !== "" && <span className="text-muted-foreground">{v.startOff}</span>}
          {!running && v.canManage && v.startOff === "" && paused && <span className="text-muted-foreground">Scans are off while the repository is paused.</span>}
        </div>
        {running && start.isError && (
          <div className="flex flex-wrap items-start gap-3 px-4 pb-3" role="note" aria-label="Start scan refused">
            <div className="min-w-0 grow">
              <ErrorBox error={start.error} />
            </div>
            <Button type="button" variant="outline" size="sm" onClick={() => start.reset()}>
              Dismiss
            </Button>
          </div>
        )}
        {cancel.isError && (
          <div className="px-4 pb-3">
            <ErrorBox error={cancel.error} />
          </div>
        )}
      </Card>
      <SecurityFindings instance={instance} project={project} />
    </>
  );
}

// LastScan is the newest ended scan: its state, what it covered and why a
// partial or failed one stopped.
function LastScan({ scan }: { scan: SecurityScanView }) {
  const [tone, label] = stateTone[scan.state];
  const facts = [
    scan.durationSeconds !== null ? duration(scan.durationSeconds) : null,
    scan.tokens ? `${count(scan.tokens.total)} tokens` : null,
    scan.branch !== "" ? scan.branch : null,
    scan.commit !== "" ? scan.commit.slice(0, 7) : null,
    scan.skillName !== "" ? `skill ${scan.skillName}` : null,
  ].filter((f): f is string => f !== null);
  return (
    <div className={lineClass}>
      <div className="flex flex-wrap items-center gap-2">
        <Pill tone={tone}>{label}</Pill>
        <span>{facts.join(" · ")}</span>
      </div>
      {(scan.state === "partial" || scan.state === "failed") && scan.reason !== "" && <div className="mt-1.5">Reason: {scan.reason}</div>}
      {scan.skillUpdated && <div className="text-muted-foreground mt-1.5">Skill updated: some findings may show as new</div>}
      {scan.usageSource === "stream" && <div className="text-muted-foreground mt-1.5">Sub-agent tokens are missing from this count.</div>}
    </div>
  );
}

// stepsText is a step limit, or the skill's own.
const stepsText = (fromSkill: boolean, n: number) => (fromSkill ? `from the skill (${n})` : String(n));

// StartScan is the Start scan button and its confirm box. The trigger is
// Radix's own button: Button, a plain function component on React 18, does
// not pass the ref the focus return needs.
function StartScan({
  v,
  start,
  open,
  setOpen,
}: {
  v: SecurityView;
  start: UseMutationResult<unknown, Error, void>;
  open: boolean;
  setOpen: (open: boolean) => void;
}) {
  return (
    <Dialog
      open={open}
      onOpenChange={(o) => {
        setOpen(o);
        if (o) start.reset();
      }}
    >
      <DialogTrigger disabled={!v.canStart} className={buttonVariants({ variant: "outline", size: "sm" })}>
        Start scan
      </DialogTrigger>
      <DialogContent showCloseButton={false}>
        <DialogHeader>
          <DialogTitle>Start a security scan</DialogTitle>
          <DialogDescription>The scan reads the default branch and runs as a Job.</DialogDescription>
        </DialogHeader>
        <StartFacts s={v.start} />
        {start.isError && <ErrorBox error={start.error} />}
        <DialogFooter>
          <DialogClose className={buttonVariants({ variant: "outline" })}>Close</DialogClose>
          <Button type="button" disabled={start.isPending} onClick={() => start.mutate()}>
            Start scan
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

// StartFacts is what a scan would use and what the last one cost.
function StartFacts({ s }: { s: SecurityStartView }) {
  return (
    <ul className="text-sm">
      <li>Model: {s.model !== "" ? s.model : "none available"}</li>
      <li>Steps, main agent: {stepsText(s.maxStepsFromSkill, s.maxSteps)}</li>
      <li>Steps, sub-agents: {stepsText(s.subagentMaxStepsFromSkill, s.subagentMaxSteps)}</li>
      <li>Timeout: {duration(s.timeoutSeconds)}</li>
      <li>
        {s.lastTokens !== null && s.lastSeconds !== null
          ? `Last scan: ${count(s.lastTokens)} tokens in ${duration(s.lastSeconds)}`
          : "No earlier scan to compare."}
      </li>
    </ul>
  );
}
